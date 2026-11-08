// Package image is Andén's content-addressed image store (D10): an OCI
// layout (blobs/sha256/<hex>) plus layers/<diffid hex>/ with every layer
// extracted once, and the overlay mount of a rootfs from those layers.
package image

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	mediaIndex     = "application/vnd.oci.image.index.v1+json"
	mediaManifest  = "application/vnd.oci.image.manifest.v1+json"
	mediaConfig    = "application/vnd.oci.image.config.v1+json"
	mediaLayerTar  = "application/vnd.oci.image.layer.v1.tar"
	mediaLayerGzip = "application/vnd.oci.image.layer.v1.tar+gzip"
	refNameKey     = "org.opencontainers.image.ref.name"
)

type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      Digest            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Platform    *struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
	} `json:"platform,omitempty"`
}

type index struct {
	Manifests []descriptor `json:"manifests"`
}

type manifest struct {
	MediaType string       `json:"mediaType"`
	Config    descriptor   `json:"config"`
	Layers    []descriptor `json:"layers"`
}

type imageConfig struct {
	Config RunConfig `json:"config"`
	RootFS struct {
		Type    string   `json:"type"`
		DiffIDs []Digest `json:"diff_ids"`
	} `json:"rootfs"`
}

// RunConfig is how the image says its process runs.
type RunConfig struct {
	Entrypoint []string `json:"Entrypoint"`
	Cmd        []string `json:"Cmd"`
	Env        []string `json:"Env"`
	WorkingDir string   `json:"WorkingDir"`
}

// Layer is one layer of an image: its blob and the digest of its
// uncompressed tar, which names its directory under layers/.
type Layer struct {
	Digest    Digest
	MediaType string
	Size      int64
	DiffID    Digest
}

// Image is a manifest with its layers, oldest first.
type Image struct {
	Digest Digest
	Config descriptor
	Run    RunConfig
	Layers []Layer
}

type Store struct {
	root string
}

// Open creates the store directories if they are missing.
func Open(root string) (*Store, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	for _, d := range []string{filepath.Join(abs, "blobs", "sha256"), filepath.Join(abs, "layers"), filepath.Join(abs, "tmp")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	marker := filepath.Join(abs, "oci-layout")
	if _, err := os.Stat(marker); errors.Is(err, fs.ErrNotExist) {
		if err := os.WriteFile(marker, []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); err != nil {
			return nil, err
		}
	}
	return &Store{root: abs}, nil
}

func (s *Store) Root() string {
	return s.root
}

func (s *Store) blobPath(d Digest) string {
	return filepath.Join(s.root, "blobs", "sha256", d.Hex())
}

// LayerPath is the directory where the layer with that diff ID is extracted.
func (s *Store) LayerPath(diffID Digest) string {
	return filepath.Join(s.root, "layers", diffID.Hex())
}

// putBlob copies a blob into the store, verifying its digest and size while
// it copies. A blob already in the store is not copied again: that is the
// deduplication.
func (s *Store) putBlob(src string, d descriptor) error {
	return s.putBlobFrom(func(Digest) (io.ReadCloser, error) { return os.Open(src) }, d)
}

// putBlobFrom is putBlob reading from whatever open returns for the digest.
func (s *Store) putBlobFrom(open func(Digest) (io.ReadCloser, error), d descriptor) error {
	dst := s.blobPath(d.Digest)
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	in, err := open(d.Digest)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "blob-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), in)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := checkBlob(d.Digest, d.Size, digestOf(h), n); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// readBlob reads a blob of the store and verifies it.
func (s *Store) readBlob(d descriptor) ([]byte, error) {
	data, err := os.ReadFile(s.blobPath(d.Digest))
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write(data)
	if err := checkBlob(d.Digest, d.Size, digestOf(h), int64(len(data))); err != nil {
		return nil, err
	}
	return data, nil
}

func (s *Store) readJSON(d descriptor, value any) error {
	data, err := s.readBlob(d)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("blob %s: %w", d.Digest, err)
	}
	return nil
}

func validDescriptor(d descriptor) error {
	if _, err := ParseDigest(string(d.Digest)); err != nil {
		return err
	}
	if d.Size < 0 {
		return fmt.Errorf("blob %s: negative size", d.Digest)
	}
	return nil
}

// readManifest reads and checks the manifest d. Its size is not known here,
// only its digest.
func (s *Store) readManifest(d Digest) (manifest, error) {
	var m manifest
	if _, err := ParseDigest(string(d)); err != nil {
		return m, err
	}
	if err := s.readJSON(descriptor{Digest: d, Size: -1}, &m); err != nil {
		return m, err
	}
	if m.MediaType != "" && m.MediaType != mediaManifest {
		return m, fmt.Errorf("manifest %s: media type %q is not %s", d, m.MediaType, mediaManifest)
	}
	if m.Config.MediaType != mediaConfig {
		return m, fmt.Errorf("manifest %s: config media type %q is not %s", d, m.Config.MediaType, mediaConfig)
	}
	if err := validDescriptor(m.Config); err != nil {
		return m, err
	}
	for _, l := range m.Layers {
		if err := validDescriptor(l); err != nil {
			return m, err
		}
		if l.MediaType != mediaLayerTar && l.MediaType != mediaLayerGzip {
			return m, fmt.Errorf("layer %s: media type %q is not supported", l.Digest, l.MediaType)
		}
	}
	return m, nil
}

// Image reads the manifest d and its config from the store, verifying both.
func (s *Store) Image(d Digest) (Image, error) {
	m, err := s.readManifest(d)
	if err != nil {
		return Image{}, err
	}
	var cfg imageConfig
	if err := s.readJSON(m.Config, &cfg); err != nil {
		return Image{}, err
	}
	if len(cfg.RootFS.DiffIDs) != len(m.Layers) {
		return Image{}, fmt.Errorf("image %s: %d layers but %d diff IDs", d, len(m.Layers), len(cfg.RootFS.DiffIDs))
	}
	img := Image{Digest: d, Config: m.Config, Run: cfg.Config}
	for i, l := range m.Layers {
		diffID, err := ParseDigest(string(cfg.RootFS.DiffIDs[i]))
		if err != nil {
			return Image{}, fmt.Errorf("image %s: %w", d, err)
		}
		img.Layers = append(img.Layers, Layer{Digest: l.Digest, MediaType: l.MediaType, Size: l.Size, DiffID: diffID})
	}
	return img, nil
}

// copyImage puts the manifest d, its config and its layers into s, reading
// each blob from blobs, a blobs/sha256 directory. Every blob copied is
// verified on the way.
func (s *Store) copyImage(blobs string, d descriptor) error {
	if err := s.putBlob(filepath.Join(blobs, d.Digest.Hex()), d); err != nil {
		return err
	}
	m, err := s.readManifest(d.Digest)
	if err != nil {
		return err
	}
	for _, b := range append([]descriptor{m.Config}, m.Layers...) {
		if err := s.putBlob(filepath.Join(blobs, b.Digest.Hex()), b); err != nil {
			return err
		}
	}
	_, err = s.Image(d.Digest)
	return err
}

// Import copies the image tagged tag in the OCI layout at dir into the store
// and returns its manifest digest. If the tag names an index, the linux/amd64
// manifest is used.
func (s *Store) Import(dir, tag string) (Digest, error) {
	data, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		return "", err
	}
	var idx index
	if err := json.Unmarshal(data, &idx); err != nil {
		return "", fmt.Errorf("%s/index.json: %w", dir, err)
	}
	var found []descriptor
	for _, m := range idx.Manifests {
		if m.Annotations[refNameKey] == tag {
			found = append(found, m)
		}
	}
	if len(found) != 1 {
		return "", fmt.Errorf("%s: %d manifests tagged %q, want 1", dir, len(found), tag)
	}
	desc := found[0]
	blobs := filepath.Join(dir, "blobs", "sha256")
	if err := validDescriptor(desc); err != nil {
		return "", err
	}
	if desc.MediaType == mediaIndex {
		if err := s.putBlob(filepath.Join(blobs, desc.Digest.Hex()), desc); err != nil {
			return "", err
		}
		var nested index
		if err := s.readJSON(desc, &nested); err != nil {
			return "", err
		}
		if desc, err = platformManifest(nested); err != nil {
			return "", fmt.Errorf("index %s: %w", found[0].Digest, err)
		}
	}
	if desc.MediaType != mediaManifest {
		return "", fmt.Errorf("%s: tag %q has media type %q", dir, tag, desc.MediaType)
	}
	if err := s.copyImage(blobs, desc); err != nil {
		return "", err
	}
	return desc.Digest, nil
}

func platformManifest(idx index) (descriptor, error) {
	for _, m := range idx.Manifests {
		if m.MediaType == mediaManifest && m.Platform != nil && m.Platform.OS == "linux" && m.Platform.Architecture == "amd64" {
			return m, validDescriptor(m)
		}
	}
	return descriptor{}, errors.New("no linux/amd64 manifest")
}

// Unpack brings the image d from src into s and extracts every layer that is
// not yet under layers/. When src is another store, each blob s lacks is
// copied and verified; a layer is verified again, against its digest and its
// diff ID, while it is extracted. An extracted layer is not verified again.
func (s *Store) Unpack(src *Store, d Digest) (Image, error) {
	if src.root != s.root {
		if _, err := ParseDigest(string(d)); err != nil {
			return Image{}, err
		}
		if err := s.copyImage(filepath.Join(src.root, "blobs", "sha256"), descriptor{Digest: d, Size: -1}); err != nil {
			return Image{}, err
		}
	}
	img, err := s.Image(d)
	if err != nil {
		return Image{}, err
	}
	for _, l := range img.Layers {
		if err := s.extractLayer(l); err != nil {
			return Image{}, err
		}
	}
	return img, nil
}

// Fetch brings the manifest d, its config and the layers s lacks through open,
// which reads a blob from another place, and verifies every blob it copies.
// Layers are extracted later, by Mount.
func (s *Store) Fetch(d Digest, open func(Digest) (io.ReadCloser, error)) (Image, error) {
	if _, err := ParseDigest(string(d)); err != nil {
		return Image{}, err
	}
	if err := s.putBlobFrom(open, descriptor{Digest: d, Size: -1}); err != nil {
		return Image{}, err
	}
	m, err := s.readManifest(d)
	if err != nil {
		return Image{}, err
	}
	for _, b := range append([]descriptor{m.Config}, m.Layers...) {
		if err := s.putBlobFrom(open, b); err != nil {
			return Image{}, err
		}
	}
	return s.Image(d)
}

// Verify hashes every blob of the store and returns how many it checked and
// the names of those that do not match.
func (s *Store) Verify() (int, []string, error) {
	dir := filepath.Join(s.root, "blobs", "sha256")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, nil, err
	}
	checked := 0
	mismatches := []string{}
	for _, e := range entries {
		checked++
		want, err := ParseDigest("sha256:" + e.Name())
		if err != nil || !e.Type().IsRegular() {
			mismatches = append(mismatches, e.Name())
			continue
		}
		got, _, err := hashFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return checked, mismatches, err
		}
		if got != want {
			mismatches = append(mismatches, string(want))
		}
	}
	return checked, mismatches, nil
}
