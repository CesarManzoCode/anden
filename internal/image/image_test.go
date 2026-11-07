package image

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

type tarEntry struct {
	name string
	kind byte
	mode int64
	link string
	data string
}

type testLayer struct {
	entries []tarEntry
	gzip    bool
}

func sha(data []byte) Digest {
	return Digest(fmt.Sprintf("sha256:%x", sha256.Sum256(data)))
}

func buildTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := tar.Header{Name: e.name, Typeflag: e.kind, Mode: e.mode, Linkname: e.link, Size: int64(len(e.data)), ModTime: time.Unix(0, 0)}
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func putTestBlob(t *testing.T, dir string, data []byte) Digest {
	t.Helper()
	d := sha(data)
	if err := os.WriteFile(filepath.Join(dir, "blobs", "sha256", d.Hex()), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return d
}

func putTestJSON(t *testing.T, dir, mediaType string, value any) descriptor {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor{MediaType: mediaType, Digest: putTestBlob(t, dir, data), Size: int64(len(data))}
}

// writeTestLayout writes an OCI layout with one manifest tagged tag and
// returns the manifest descriptor.
func writeTestLayout(t *testing.T, dir, tag string, layers []testLayer) descriptor {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	var m manifest
	m.MediaType = mediaManifest
	var cfg imageConfig
	cfg.RootFS.Type = "layers"
	for _, l := range layers {
		raw := buildTar(t, l.entries)
		blob, mediaType := raw, mediaLayerTar
		if l.gzip {
			var buf bytes.Buffer
			zw := gzip.NewWriter(&buf)
			zw.Write(raw)
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
			blob, mediaType = buf.Bytes(), mediaLayerGzip
		}
		m.Layers = append(m.Layers, descriptor{MediaType: mediaType, Digest: putTestBlob(t, dir, blob), Size: int64(len(blob))})
		cfg.RootFS.DiffIDs = append(cfg.RootFS.DiffIDs, sha(raw))
	}
	m.Config = putTestJSON(t, dir, mediaConfig, cfg)
	man := putTestJSON(t, dir, mediaManifest, m)
	man.Annotations = map[string]string{refNameKey: tag}
	idx, err := json.Marshal(index{Manifests: []descriptor{man}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), idx, 0o644); err != nil {
		t.Fatal(err)
	}
	return man
}

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// skipWithoutPrivilege skips when whiteouts or user xattrs cannot be created
// here: mknod of 0/0 needs to be root of a user namespace.
func skipWithoutPrivilege(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ENOTSUP) {
		t.Skipf("needs mknod 0/0 and user xattrs: %v", err)
	}
}

var plainLayer = testLayer{entries: []tarEntry{
	{name: "etc/", kind: tar.TypeDir, mode: 0o755},
	{name: "etc/hostname", kind: tar.TypeReg, mode: 0o644, data: "anden\n"},
}, gzip: true}

func TestImportVerifiesDigest(t *testing.T) {
	layout := t.TempDir()
	man := writeTestLayout(t, layout, "v1", []testLayer{plainLayer})
	s := openStore(t)
	got, err := s.Import(layout, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if got != man.Digest {
		t.Fatalf("Import returned %s, want %s", got, man.Digest)
	}
	checked, mismatches, err := s.Verify()
	if err != nil || checked != 3 || len(mismatches) != 0 {
		t.Fatalf("Verify = %d, %v, %v; want 3 blobs and no mismatch", checked, mismatches, err)
	}

	// The same layout with one byte of the layer changed must be refused.
	var m manifest
	data, err := os.ReadFile(filepath.Join(layout, "blobs", "sha256", man.Digest.Hex()))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	layerPath := filepath.Join(layout, "blobs", "sha256", m.Layers[0].Digest.Hex())
	blob, err := os.ReadFile(layerPath)
	if err != nil {
		t.Fatal(err)
	}
	blob[len(blob)/2] ^= 0xff
	if err := os.WriteFile(layerPath, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(t).Import(layout, "v1"); !errors.Is(err, ErrMismatch) {
		t.Fatalf("Import of a corrupted layer: %v, want ErrMismatch", err)
	}

	// A blob corrupted inside the store is found by Verify and refused on transfer.
	stored := s.blobPath(m.Layers[0].Digest)
	if err := os.WriteFile(stored, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	_, mismatches, err = s.Verify()
	if err != nil || len(mismatches) != 1 || mismatches[0] != string(m.Layers[0].Digest) {
		t.Fatalf("Verify after corruption = %v, %v; want [%s]", mismatches, err, m.Layers[0].Digest)
	}
	if _, err := openStore(t).Unpack(s, man.Digest); !errors.Is(err, ErrMismatch) {
		t.Fatalf("Unpack of a corrupted blob: %v, want ErrMismatch", err)
	}
}

func TestFetchVerifiesEveryBlob(t *testing.T) {
	layout := t.TempDir()
	man := writeTestLayout(t, layout, "v1", []testLayer{plainLayer})
	blobs := filepath.Join(layout, "blobs", "sha256")
	open := func(d Digest) (io.ReadCloser, error) { return os.Open(filepath.Join(blobs, d.Hex())) }

	s := openStore(t)
	img, err := s.Fetch(man.Digest, open)
	if err != nil {
		t.Fatal(err)
	}
	if img.Digest != man.Digest || len(img.Layers) != 1 {
		t.Fatalf("Fetch = %+v", img)
	}
	if checked, mismatches, err := s.Verify(); err != nil || checked != 3 || len(mismatches) != 0 {
		t.Fatalf("Verify = %d, %v, %v", checked, mismatches, err)
	}
	// What is already there is not asked for again.
	if _, err := s.Fetch(man.Digest, func(d Digest) (io.ReadCloser, error) { return nil, fmt.Errorf("asked for %s", d) }); err != nil {
		t.Fatal(err)
	}

	layer := filepath.Join(blobs, img.Layers[0].Digest.Hex())
	raw, err := os.ReadFile(layer)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] ^= 0xff
	if err := os.WriteFile(layer, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(t).Fetch(man.Digest, open); !errors.Is(err, ErrMismatch) {
		t.Fatalf("a changed layer must be refused, got %v", err)
	}
}

func TestImageRunConfig(t *testing.T) {
	layout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(layout, "blobs", "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := buildTar(t, plainLayer.entries)
	layerDigest := putTestBlob(t, layout, raw)
	var cfg imageConfig
	cfg.Config = RunConfig{Entrypoint: []string{"/e"}, Cmd: []string{"c"}, Env: []string{"A=1"}, WorkingDir: "/w"}
	cfg.RootFS.Type = "layers"
	cfg.RootFS.DiffIDs = []Digest{sha(raw)}
	m := manifest{MediaType: mediaManifest, Config: putTestJSON(t, layout, mediaConfig, cfg),
		Layers: []descriptor{{MediaType: mediaLayerTar, Digest: layerDigest, Size: int64(len(raw))}}}
	man := putTestJSON(t, layout, mediaManifest, m)
	s := openStore(t)
	img, err := s.Fetch(man.Digest, func(d Digest) (io.ReadCloser, error) {
		return os.Open(filepath.Join(layout, "blobs", "sha256", d.Hex()))
	})
	if err != nil {
		t.Fatal(err)
	}
	if img.Run.Entrypoint[0] != "/e" || img.Run.Cmd[0] != "c" || img.Run.Env[0] != "A=1" || img.Run.WorkingDir != "/w" {
		t.Fatalf("Run = %+v", img.Run)
	}
}

func TestParseDigest(t *testing.T) {
	good := "sha256:" + fmt.Sprintf("%064x", 1)
	if _, err := ParseDigest(good); err != nil {
		t.Errorf("ParseDigest(%s): %v", good, err)
	}
	for _, bad := range []string{"", "sha256:abc", "sha512:" + fmt.Sprintf("%064x", 1), "sha256:" + fmt.Sprintf("%064X", 0xab), "sha256:../" + fmt.Sprintf("%061x", 1)} {
		if _, err := ParseDigest(bad); err == nil {
			t.Errorf("ParseDigest(%q) accepted", bad)
		}
	}
}

func TestExtractWhiteoutAndOpaque(t *testing.T) {
	root := t.TempDir()
	layer := buildTar(t, []tarEntry{
		{name: "data/", kind: tar.TypeDir, mode: 0o755},
		{name: "data/.wh.x", kind: tar.TypeReg, mode: 0o644},
		{name: "opaque/", kind: tar.TypeDir, mode: 0o750},
		{name: "opaque/.wh..wh..opq", kind: tar.TypeReg, mode: 0o644},
		{name: "opaque/new", kind: tar.TypeReg, mode: 0o4755, data: "new\n"},
		{name: "dev/null", kind: tar.TypeChar, mode: 0o666},
	})
	err := extract(bytes.NewReader(layer), root)
	skipWithoutPrivilege(t, err)
	if err != nil {
		t.Fatal(err)
	}
	var st syscall.Stat_t
	if err := syscall.Lstat(filepath.Join(root, "data", "x"), &st); err != nil {
		t.Fatal(err)
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFCHR || st.Rdev != 0 {
		t.Errorf("whiteout is mode %o rdev %d, want a char device 0/0", st.Mode, st.Rdev)
	}
	if _, err := os.Lstat(filepath.Join(root, "data", ".wh.x")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the .wh. marker itself was written: %v", err)
	}
	buf := make([]byte, 8)
	if n, err := syscall.Getxattr(filepath.Join(root, "opaque"), opaqueXattr, buf); err != nil || string(buf[:n]) != "y" {
		t.Errorf("opaque xattr: %v, %q; want \"y\"", err, buf[:max(n, 0)])
	}
	if _, err := os.Lstat(filepath.Join(root, "opaque", whiteoutOpaque)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the opaque marker itself was written: %v", err)
	}
	if err := syscall.Lstat(filepath.Join(root, "opaque"), &st); err != nil || st.Mode&0o7777 != 0o750 {
		t.Errorf("opaque/ mode %o, %v; want 750", st.Mode&0o7777, err)
	}
	if err := syscall.Lstat(filepath.Join(root, "opaque", "new"), &st); err != nil || st.Mode&0o7777 != 0o4755 {
		t.Errorf("opaque/new mode %o, %v; want 4755", st.Mode&0o7777, err)
	}
	// A device becomes an empty file with its mode, as umoci --rootless does.
	if err := syscall.Lstat(filepath.Join(root, "dev", "null"), &st); err != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Size != 0 || st.Mode&0o7777 != 0o666 {
		t.Errorf("dev/null is mode %o size %d, %v; want an empty regular file 666", st.Mode, st.Size, err)
	}
}

func TestWhiteoutKeepsEntriesOfSameLayer(t *testing.T) {
	root := t.TempDir()
	layer := buildTar(t, []tarEntry{
		{name: "file", kind: tar.TypeReg, mode: 0o644, data: "kept\n"},
		{name: ".wh.file", kind: tar.TypeReg, mode: 0o644},
		{name: "dir/", kind: tar.TypeDir, mode: 0o755},
		{name: "dir/new", kind: tar.TypeReg, mode: 0o644, data: "new\n"},
		{name: ".wh.dir", kind: tar.TypeReg, mode: 0o644},
		{name: ".wh.later", kind: tar.TypeReg, mode: 0o644},
		{name: "later/", kind: tar.TypeDir, mode: 0o755},
	})
	err := extract(bytes.NewReader(layer), root)
	skipWithoutPrivilege(t, err)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"file": "kept\n", "dir/new": "new\n"} {
		got, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
	// A directory next to its whiteout must not merge with the lower layers.
	for _, name := range []string{"dir", "later"} {
		buf := make([]byte, 8)
		if n, err := syscall.Getxattr(filepath.Join(root, name), opaqueXattr, buf); err != nil || string(buf[:n]) != "y" {
			t.Errorf("%s opaque xattr: %v, %q; want \"y\"", name, err, buf[:max(n, 0)])
		}
	}
}

func TestDirReplacedBySymlinkKeepsChmodInside(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(filepath.Join(outside, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(outside, "b"), 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "layer")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	layer := buildTar(t, []tarEntry{
		{name: "a/", kind: tar.TypeDir, mode: 0o755},
		{name: "a/b/", kind: tar.TypeDir, mode: 0o777},
		{name: "a", kind: tar.TypeSymlink, link: outside},
	})
	if err := extract(bytes.NewReader(layer), root); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(outside, "b"))
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("outside/b: %v, %v; want mode 700 untouched", fi, err)
	}
}

func TestExtractRejectsEscapes(t *testing.T) {
	cases := map[string][]tarEntry{
		"dotdot": {{name: "../evil", kind: tar.TypeReg, mode: 0o644, data: "x"}},
		"nested dotdot": {
			{name: "a/", kind: tar.TypeDir, mode: 0o755},
			{name: "a/../../evil", kind: tar.TypeReg, mode: 0o644, data: "x"},
		},
		"symlink parent": {
			{name: "link", kind: tar.TypeSymlink, link: "/"},
			{name: "link/evil", kind: tar.TypeReg, mode: 0o644, data: "x"},
		},
		"relative symlink parent": {
			{name: "up", kind: tar.TypeSymlink, link: "../.."},
			{name: "up/evil", kind: tar.TypeReg, mode: 0o644, data: "x"},
		},
		"hardlink out": {{name: "hard", kind: tar.TypeLink, link: "../evil"}},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "layer", "root")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			err := extract(bytes.NewReader(buildTar(t, entries)), root)
			if !errors.Is(err, ErrEscape) {
				t.Fatalf("extract: %v, want ErrEscape", err)
			}
			for _, p := range []string{filepath.Join(base, "evil"), filepath.Join(base, "layer", "evil"), "/evil"} {
				if _, err := os.Lstat(p); err == nil {
					t.Errorf("%s was written outside the layer", p)
				}
			}
		})
	}
}

func TestSharedLayerStoredOnce(t *testing.T) {
	shared := plainLayer
	own := testLayer{entries: []tarEntry{{name: "app", kind: tar.TypeReg, mode: 0o755, data: "app\n"}}}
	layoutA, layoutB := t.TempDir(), t.TempDir()
	manA := writeTestLayout(t, layoutA, "a", []testLayer{shared})
	manB := writeTestLayout(t, layoutB, "b", []testLayer{shared, own})
	s := openStore(t)
	if _, err := s.Import(layoutA, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Import(layoutB, "b"); err != nil {
		t.Fatal(err)
	}
	imgA, err := s.Image(manA.Digest)
	if err != nil {
		t.Fatal(err)
	}
	imgB, err := s.Image(manB.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if imgA.Layers[0].Digest != imgB.Layers[0].Digest {
		t.Fatalf("the shared layer has two digests: %s and %s", imgA.Layers[0].Digest, imgB.Layers[0].Digest)
	}
	// Two manifests, two configs and two distinct layers.
	entries, err := os.ReadDir(filepath.Join(s.Root(), "blobs", "sha256"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 6 {
		t.Errorf("%d blobs in the store, want 6", len(entries))
	}
	sharedData, err := os.ReadFile(s.blobPath(imgA.Layers[0].Digest))
	if err != nil {
		t.Fatal(err)
	}
	copies := 0
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(s.Root(), "blobs", "sha256", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(data, sharedData) {
			copies++
		}
	}
	if copies != 1 {
		t.Errorf("the shared layer is stored %d times, want 1", copies)
	}

	// Each layer is extracted once, under its diff ID.
	if _, err := s.Unpack(s, manA.Digest); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Unpack(s, manB.Digest); err != nil {
		t.Fatal(err)
	}
	layers, err := os.ReadDir(filepath.Join(s.Root(), "layers"))
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 2 {
		t.Errorf("%d extracted layers, want 2", len(layers))
	}
	content, err := os.ReadFile(filepath.Join(s.LayerPath(imgA.Layers[0].DiffID), "etc", "hostname"))
	if err != nil || string(content) != "anden\n" {
		t.Errorf("extracted etc/hostname = %q, %v", content, err)
	}
	// The layer directory is / of the rootfs: 0700 would lock out non-root users.
	fi, err := os.Stat(s.LayerPath(imgA.Layers[0].DiffID))
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("layer directory: %v, %v; want mode 755", fi, err)
	}
}
