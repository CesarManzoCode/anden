package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	mediaIndex     = "application/vnd.oci.image.index.v1+json"
	mediaManifest  = "application/vnd.oci.image.manifest.v1+json"
	mediaConfig    = "application/vnd.oci.image.config.v1+json"
	mediaLayerTar  = "application/vnd.oci.image.layer.v1.tar"
	mediaLayerGzip = "application/vnd.oci.image.layer.v1.tar+gzip"
	refNameKey     = "org.opencontainers.image.ref.name"
)

// layerTime is the only timestamp in a layout, so the same input gives the same digests.
var layerTime = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type index struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Manifests     []descriptor `json:"manifests"`
}

type manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        descriptor   `json:"config"`
	Layers        []descriptor `json:"layers"`
}

type imageConfig struct {
	Created      string    `json:"created"`
	Architecture string    `json:"architecture"`
	OS           string    `json:"os"`
	Config       runConfig `json:"config"`
	RootFS       rootFS    `json:"rootfs"`
}

type runConfig struct {
	Cmd []string `json:"Cmd,omitempty"`
	Env []string `json:"Env,omitempty"`
}

type rootFS struct {
	Type    string   `json:"type"`
	DiffIDs []string `json:"diff_ids"`
}

type entry struct {
	name string
	kind byte
	mode int64
	link string
	data []byte
}

type layerSpec struct {
	entries  []entry
	compress bool
}

type image struct {
	layers []layerSpec
	cmd    []string
	env    []string
}

func dirEntry(name string, mode int64) entry {
	return entry{name: name + "/", kind: tar.TypeDir, mode: mode}
}

func fileEntry(name string, mode int64, content []byte) entry {
	return entry{name: name, kind: tar.TypeReg, mode: mode, data: content}
}

func textEntry(name string, mode int64, content string) entry {
	return fileEntry(name, mode, []byte(content))
}

func symlinkEntry(name, target string) entry {
	return entry{name: name, kind: tar.TypeSymlink, mode: 0o777, link: target}
}

func hardlinkEntry(name, target string, mode int64) entry {
	return entry{name: name, kind: tar.TypeLink, mode: mode, link: target}
}

func digestOf(data []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(data))
}

// buildLayer returns the layer blob and the digest of its uncompressed tar, its diff ID.
func buildLayer(spec layerSpec) (blob []byte, diffID string, err error) {
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, e := range spec.entries {
		hdr := tar.Header{Typeflag: e.kind, Name: e.name, Linkname: e.link, Mode: e.mode, Size: int64(len(e.data)), ModTime: layerTime}
		if err := tw.WriteHeader(&hdr); err != nil {
			return nil, "", fmt.Errorf("%s: %w", e.name, err)
		}
		if _, err := tw.Write(e.data); err != nil {
			return nil, "", fmt.Errorf("%s: %w", e.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, "", err
	}
	diffID = digestOf(raw.Bytes())
	if !spec.compress {
		return raw.Bytes(), diffID, nil
	}
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		return nil, "", err
	}
	if err := zw.Close(); err != nil {
		return nil, "", err
	}
	return compressed.Bytes(), diffID, nil
}

func writeBlob(dir, mediaType string, data []byte) (descriptor, error) {
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	if err := os.WriteFile(filepath.Join(dir, "blobs", "sha256", digest), data, 0o644); err != nil {
		return descriptor{}, err
	}
	return descriptor{MediaType: mediaType, Digest: "sha256:" + digest, Size: int64(len(data))}, nil
}

func writeJSONBlob(dir, mediaType string, value any) (descriptor, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return descriptor{}, err
	}
	return writeBlob(dir, mediaType, data)
}

// writeLayout creates an OCI layout in dir and returns the manifest digest. It
// refuses a directory that already holds a layout.
func writeLayout(dir, tag string, img image) (string, error) {
	if _, err := os.Stat(filepath.Join(dir, "index.json")); err == nil {
		return "", fmt.Errorf("%s already holds a layout", dir)
	}
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o755); err != nil {
		return "", err
	}
	layers := make([]descriptor, 0, len(img.layers))
	diffIDs := make([]string, 0, len(img.layers))
	for _, spec := range img.layers {
		blob, diffID, err := buildLayer(spec)
		if err != nil {
			return "", err
		}
		mediaType := mediaLayerTar
		if spec.compress {
			mediaType = mediaLayerGzip
		}
		layer, err := writeBlob(dir, mediaType, blob)
		if err != nil {
			return "", err
		}
		layers = append(layers, layer)
		diffIDs = append(diffIDs, diffID)
	}
	cfg := imageConfig{Created: layerTime.Format(time.RFC3339), Architecture: "amd64", OS: "linux"}
	cfg.Config = runConfig{Cmd: img.cmd, Env: img.env}
	cfg.RootFS = rootFS{Type: "layers", DiffIDs: diffIDs}
	cfgDesc, err := writeJSONBlob(dir, mediaConfig, cfg)
	if err != nil {
		return "", err
	}
	man, err := writeJSONBlob(dir, mediaManifest, manifest{SchemaVersion: 2, MediaType: mediaManifest, Config: cfgDesc, Layers: layers})
	if err != nil {
		return "", err
	}
	man.Annotations = map[string]string{refNameKey: tag}
	idx, err := json.Marshal(index{SchemaVersion: 2, MediaType: mediaIndex, Manifests: []descriptor{man}})
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), idx, 0o644); err != nil {
		return "", err
	}
	return man.Digest, nil
}
