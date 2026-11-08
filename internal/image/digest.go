package image

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"strings"
)

// ErrMismatch marks a blob whose content does not match its digest or size.
var ErrMismatch = errors.New("digest mismatch")

// Digest is "sha256:<64 lowercase hex>"; D10 admits no other algorithm.
type Digest string

func ParseDigest(s string) (Digest, error) {
	h, ok := strings.CutPrefix(s, "sha256:")
	if !ok || len(h) != sha256.Size*2 {
		return "", fmt.Errorf("digest %q is not sha256:<64 hex>", s)
	}
	for _, c := range h {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", fmt.Errorf("digest %q is not sha256:<64 hex>", s)
		}
	}
	return Digest(s), nil
}

// Hex is the digest without its algorithm, the file name under blobs/sha256/.
func (d Digest) Hex() string {
	return strings.TrimPrefix(string(d), "sha256:")
}

func digestOf(h hash.Hash) Digest {
	return Digest("sha256:" + hex.EncodeToString(h.Sum(nil)))
}

// checkBlob compares what was read against the descriptor. A size below 0 is
// not checked.
func checkBlob(want Digest, wantSize int64, got Digest, gotSize int64) error {
	if wantSize >= 0 && gotSize != wantSize {
		return fmt.Errorf("blob %s: %w: %d bytes, descriptor says %d", want, ErrMismatch, gotSize, wantSize)
	}
	if got != want {
		return fmt.Errorf("blob %s: %w: content hashes to %s", want, ErrMismatch, got)
	}
	return nil
}

func hashFile(path string) (Digest, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return digestOf(h), n, nil
}
