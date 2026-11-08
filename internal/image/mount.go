package image

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Mount mounts the rootfs of image d at dest with overlay and userxattr,
// newest layer first, extracting the layers that are missing. Without upper
// the mount is read-only, and a single layer goes over an empty directory so
// that the stack keeps two lower layers. upper and work go together: they are
// the writable layer of an instance.
func (s *Store) Mount(d Digest, dest, upper, work string) error {
	if (upper == "") != (work == "") {
		return errors.New("overlay needs both upper and work, or neither")
	}
	img, err := s.Unpack(s, d)
	if err != nil {
		return err
	}
	if len(img.Layers) == 0 {
		return fmt.Errorf("image %s has no layers", d)
	}
	lowers := make([]string, 0, len(img.Layers)+1)
	for i := len(img.Layers) - 1; i >= 0; i-- {
		lowers = append(lowers, s.LayerPath(img.Layers[i].DiffID))
	}
	if upper == "" && len(lowers) == 1 {
		empty := filepath.Join(s.root, "empty")
		if err := os.MkdirAll(empty, 0o755); err != nil {
			return err
		}
		lowers = append(lowers, empty)
	}
	data := "lowerdir=" + strings.Join(lowers, ":") + ",userxattr"
	if upper != "" {
		data += ",upperdir=" + upper + ",workdir=" + work
	}
	for _, p := range append(lowers, upper, work) {
		if strings.ContainsAny(p, ":,") {
			return fmt.Errorf("overlay path %q contains ':' or ','", p)
		}
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	if err := syscall.Mount("overlay", dest, "overlay", 0, data); err != nil {
		return fmt.Errorf("mount overlay on %s: %w", dest, err)
	}
	return nil
}
