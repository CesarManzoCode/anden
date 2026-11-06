package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type parsedImage struct {
	digest string
	config imageConfig
	layers []map[string]*tar.Header
}

func readBlob(t *testing.T, dir string, d descriptor) []byte {
	t.Helper()
	hex, ok := strings.CutPrefix(d.Digest, "sha256:")
	if !ok {
		t.Fatalf("digest without algorithm: %s", d.Digest)
	}
	data, err := os.ReadFile(filepath.Join(dir, "blobs", "sha256", hex))
	if err != nil {
		t.Fatal(err)
	}
	if got := digestOf(data); got != d.Digest {
		t.Errorf("blob %s hashes to %s", d.Digest, got)
	}
	if int64(len(data)) != d.Size {
		t.Errorf("blob %s has %d bytes, descriptor says %d", d.Digest, len(data), d.Size)
	}
	return data
}

func readJSON(t *testing.T, data []byte, value any) {
	t.Helper()
	if err := json.Unmarshal(data, value); err != nil {
		t.Fatal(err)
	}
}

func readLayer(t *testing.T, blob []byte, mediaType string) (map[string]*tar.Header, []byte) {
	t.Helper()
	raw := blob
	if mediaType == mediaLayerGzip {
		zr, err := gzip.NewReader(bytes.NewReader(blob))
		if err != nil {
			t.Fatal(err)
		}
		if raw, err = io.ReadAll(zr); err != nil {
			t.Fatal(err)
		}
	} else if mediaType != mediaLayerTar {
		t.Fatalf("unexpected layer media type %s", mediaType)
	}
	headers := map[string]*tar.Header{}
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return headers, raw
		}
		if err != nil {
			t.Fatal(err)
		}
		headers[hdr.Name] = hdr
	}
}

// readLayout checks the whole layout against its descriptors and returns the image under tag.
func readLayout(t *testing.T, dir, tag string) parsedImage {
	t.Helper()
	marker, err := os.ReadFile(filepath.Join(dir, "oci-layout"))
	if err != nil {
		t.Fatal(err)
	}
	var version struct {
		ImageLayoutVersion string `json:"imageLayoutVersion"`
	}
	readJSON(t, marker, &version)
	if version.ImageLayoutVersion != "1.0.0" {
		t.Errorf("imageLayoutVersion is %q", version.ImageLayoutVersion)
	}
	idxData, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var idx index
	readJSON(t, idxData, &idx)
	if idx.SchemaVersion != 2 || idx.MediaType != mediaIndex || len(idx.Manifests) != 1 {
		t.Fatalf("bad index: %s", idxData)
	}
	desc := idx.Manifests[0]
	if desc.MediaType != mediaManifest || desc.Annotations[refNameKey] != tag {
		t.Fatalf("bad manifest descriptor: %+v", desc)
	}
	var man manifest
	readJSON(t, readBlob(t, dir, desc), &man)
	if man.SchemaVersion != 2 || man.MediaType != mediaManifest || man.Config.MediaType != mediaConfig {
		t.Fatalf("bad manifest: %+v", man)
	}
	img := parsedImage{digest: desc.Digest}
	readJSON(t, readBlob(t, dir, man.Config), &img.config)
	if img.config.RootFS.Type != "layers" || len(img.config.RootFS.DiffIDs) != len(man.Layers) {
		t.Fatalf("bad rootfs: %+v", img.config.RootFS)
	}
	for i, layer := range man.Layers {
		headers, raw := readLayer(t, readBlob(t, dir, layer), layer.MediaType)
		if got := digestOf(raw); got != img.config.RootFS.DiffIDs[i] {
			t.Errorf("layer %d: diff ID is %s, config says %s", i, got, img.config.RootFS.DiffIDs[i])
		}
		img.layers = append(img.layers, headers)
	}
	blobs, err := os.ReadDir(filepath.Join(dir, "blobs", "sha256"))
	if err != nil {
		t.Fatal(err)
	}
	if want := len(man.Layers) + 2; len(blobs) != want {
		t.Errorf("layout has %d blobs, want %d", len(blobs), want)
	}
	return img
}

func TestSyntheticLayout(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "layout")
	var stdout bytes.Buffer
	if err := run([]string{"synthetic", "--out", dir, "--tag", "t1"}, &stdout); err != nil {
		t.Fatal(err)
	}
	img := readLayout(t, dir, "t1")
	if got := strings.TrimSpace(stdout.String()); got != img.digest {
		t.Errorf("printed %s, manifest is %s", got, img.digest)
	}
	if len(img.layers) != 2 {
		t.Fatalf("got %d layers, want 2", len(img.layers))
	}
	lower, upper := img.layers[0], img.layers[1]
	for _, name := range []string{"data/.wh.x", ".wh.gone", "opaque/.wh..wh..opq"} {
		hdr, ok := upper[name]
		if !ok || hdr.Typeflag != tar.TypeReg {
			t.Errorf("upper layer lacks the marker %s", name)
		}
	}
	if hdr := lower["etc/config.hard"]; hdr == nil || hdr.Typeflag != tar.TypeLink || hdr.Linkname != "etc/config" {
		t.Errorf("bad hardlink: %+v", hdr)
	}
	if hdr := lower["etc/config.link"]; hdr == nil || hdr.Typeflag != tar.TypeSymlink || hdr.Linkname != "config" {
		t.Errorf("bad symlink: %+v", hdr)
	}
	if hdr := lower["sticky/"]; hdr == nil || hdr.Typeflag != tar.TypeDir || hdr.Mode != 0o1777 {
		t.Errorf("bad sticky directory: %+v", hdr)
	}
	if hdr := lower["bin/setuid"]; hdr == nil || hdr.Mode != 0o4755 {
		t.Errorf("bad setuid file: %+v", hdr)
	}
	if hdr := lower["bin/setgid"]; hdr == nil || hdr.Mode != 0o2755 {
		t.Errorf("bad setgid file: %+v", hdr)
	}
}

func TestSyntheticIsDeterministic(t *testing.T) {
	base := t.TempDir()
	var first, second bytes.Buffer
	if err := run([]string{"synthetic", "--out", filepath.Join(base, "a")}, &first); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"synthetic", "--out", filepath.Join(base, "b")}, &second); err != nil {
		t.Fatal(err)
	}
	if first.String() != second.String() {
		t.Errorf("digests differ: %s and %s", first.String(), second.String())
	}
}

func TestRefusesAnExistingLayout(t *testing.T) {
	dir := t.TempDir()
	if err := run([]string{"synthetic", "--out", dir}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"synthetic", "--out", dir}, io.Discard); err == nil {
		t.Error("a second layout over the first was accepted")
	}
}

func TestBadUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {"synthetic"}, {"pack", "--out", t.TempDir()}} {
		if err := run(args, io.Discard); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
}

func TestParseLdd(t *testing.T) {
	output := "\tlinux-vdso.so.1 (0x00007f7f03040000)\n" +
		"\tlibm.so.6 => /usr/lib/libm.so.6 (0x00007f7f0297f000)\n" +
		"\t/lib64/ld-linux-x86-64.so.2 => /usr/lib64/ld-linux-x86-64.so.2 (0x00007f7f03042000)\n" +
		"\tlibz.so.1 => /usr/lib/libz.so.1 (0x00007f7f02fce000)\n"
	want := []string{"/lib64/ld-linux-x86-64.so.2", "/usr/lib/libm.so.6", "/usr/lib/libz.so.1", "/usr/lib64/ld-linux-x86-64.so.2"}
	got, err := parseLdd(output)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	for _, static := range []string{"\tnot a dynamic executable\n", "\tstatically linked\n"} {
		if libs, err := parseLdd(static); err != nil || len(libs) != 0 {
			t.Errorf("%q: got %v, %v", static, libs, err)
		}
	}
	if _, err := parseLdd("\tlibgone.so.1 => not found\n"); err == nil {
		t.Error("a missing library was accepted")
	}
}

func TestPackLayout(t *testing.T) {
	base := t.TempDir()
	bin := filepath.Join(base, "service")
	lib := filepath.Join(base, "lib", "libfoo.so.1")
	if err := os.Mkdir(filepath.Dir(lib), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lib, []byte("library"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, err := packImage(bin, []string{lib}, []string{"/usr/bin/service", "--flag"}, []string{"A=1"})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "layout")
	if _, err := writeLayout(dir, "t1", spec); err != nil {
		t.Fatal(err)
	}
	img := readLayout(t, dir, "t1")
	if !reflect.DeepEqual(img.config.Config.Cmd, []string{"/usr/bin/service", "--flag"}) {
		t.Errorf("Cmd is %v", img.config.Config.Cmd)
	}
	if !reflect.DeepEqual(img.config.Config.Env, []string{"A=1"}) {
		t.Errorf("Env is %v", img.config.Config.Env)
	}
	if len(img.layers) != 1 {
		t.Fatalf("got %d layers, want 1", len(img.layers))
	}
	headers := img.layers[0]
	if hdr := headers["usr/bin/service"]; hdr == nil || hdr.Mode != 0o755 || hdr.Size != int64(len("binary")) {
		t.Errorf("bad binary entry: %+v", hdr)
	}
	libName := strings.TrimPrefix(lib, "/")
	if hdr := headers[libName]; hdr == nil || hdr.Mode != 0o644 {
		t.Errorf("bad library entry: %+v", hdr)
	}
	for _, name := range []string{"usr/", "usr/bin/", filepath.Dir(libName) + "/", "dev/", "proc/", "sys/"} {
		if hdr := headers[name]; hdr == nil || hdr.Typeflag != tar.TypeDir {
			t.Errorf("missing directory %s", name)
		}
	}
	if hdr := headers["tmp/"]; hdr == nil || hdr.Mode != 0o1777 {
		t.Errorf("bad tmp directory: %+v", hdr)
	}
}

func TestPackDefaultCmdIsTheBinary(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "service")
	if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	spec, err := packImage(bin, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(spec.cmd, []string{"/usr/bin/service"}) {
		t.Errorf("Cmd is %v", spec.cmd)
	}
}
