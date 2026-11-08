package image

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

const (
	whiteoutPrefix = ".wh."
	whiteoutMeta   = ".wh..wh."
	whiteoutOpaque = ".wh..wh..opq"
	opaqueXattr    = "user.overlay.opaque"
)

// ErrEscape marks a layer entry that would be written outside the layer.
var ErrEscape = errors.New("path escapes the layer")

// extractLayer extracts l into layers/<diffid hex>/ unless it is there
// already. It works in a temporary directory and renames it only when both
// the blob digest and the diff ID match.
func (s *Store) extractLayer(l Layer) error {
	dest := s.LayerPath(l.DiffID)
	if _, err := os.Lstat(dest); err == nil {
		return nil
	}
	f, err := os.Open(s.blobPath(l.Digest))
	if err != nil {
		return err
	}
	defer f.Close()
	tmp, err := os.MkdirTemp(filepath.Join(s.root, "tmp"), "layer-")
	if err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			forceRemove(tmp)
		}
	}()
	// MkdirTemp makes it 0700, and this directory is what the rootfs shows as /.
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}

	blobHash := sha256.New()
	blob := &countingReader{r: io.TeeReader(f, blobHash)}
	var r io.Reader = blob
	if l.MediaType == mediaLayerGzip {
		zr, err := gzip.NewReader(blob)
		if err != nil {
			return fmt.Errorf("layer %s: %w", l.Digest, err)
		}
		defer zr.Close()
		r = zr
	}
	diffHash := sha256.New()
	tarStream := io.TeeReader(r, diffHash)
	extractErr := extract(tarStream, tmp)
	// Both hashes must cover the whole stream, not only up to the tar end.
	if extractErr == nil {
		_, extractErr = io.Copy(io.Discard, tarStream)
	}
	if _, err := io.Copy(io.Discard, blob); err != nil {
		return err
	}
	// A digest mismatch explains a broken stream better than the tar error.
	if err := checkBlob(l.Digest, l.Size, digestOf(blobHash), blob.n); err != nil {
		return err
	}
	if extractErr != nil {
		return fmt.Errorf("layer %s: %w", l.Digest, extractErr)
	}
	if got := digestOf(diffHash); got != l.DiffID {
		return fmt.Errorf("layer %s: %w: diff ID %s, uncompressed content hashes to %s", l.Digest, ErrMismatch, l.DiffID, got)
	}
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	done = true
	return nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// forceRemove deletes a tree whose directories may lack write permission.
func forceRemove(dir string) error {
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(dir)
}

// cleanName turns a tar entry name into a path relative to the layer root,
// "" for the root itself. A name that climbs out of the layer is refused.
func cleanName(name string) (string, error) {
	n := path.Clean(strings.TrimLeft(name, "/"))
	if n == "." {
		return "", nil
	}
	if n == ".." || strings.HasPrefix(n, "../") {
		return "", fmt.Errorf("%w: %q", ErrEscape, name)
	}
	return n, nil
}

// walkParents checks every component of dir under root and, with create,
// makes the missing ones. A component that is a symlink is refused whether
// its target escapes or not: following it safely would need the merged view
// of every layer, and a layer written by an image builder never needs it.
func walkParents(root, dir string, create bool) error {
	cur := root
	for _, part := range strings.Split(dir, "/") {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		switch {
		case errors.Is(err, fs.ErrNotExist) && create:
			if err := os.Mkdir(cur, 0o755); err != nil {
				return err
			}
		case err != nil:
			return err
		case fi.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("%w: %s goes through a symlink", ErrEscape, dir)
		case !fi.IsDir():
			return fmt.Errorf("%s: not a directory", dir)
		}
	}
	return nil
}

// extract writes a layer tar under root in the form overlay reads: a
// whiteout becomes a char device 0/0 and an opaque marker the xattr
// user.overlay.opaque on its directory. Owners are not applied, like umoci
// --rootless, and neither are extended attributes or times.
func extract(r io.Reader, root string) error {
	tr := tar.NewReader(r)
	dirModes := map[string]uint32{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		rel, err := cleanName(hdr.Name)
		if err != nil {
			return err
		}
		if rel == "" {
			continue
		}
		dir, base := path.Split(rel)
		if err := walkParents(root, dir, true); err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		parent := filepath.Join(root, filepath.FromSlash(dir))
		switch {
		case base == whiteoutOpaque:
			if err := syscall.Setxattr(parent, opaqueXattr, []byte("y"), 0); err != nil {
				return fmt.Errorf("%s: setxattr %s: %w", rel, opaqueXattr, err)
			}
		case strings.HasPrefix(base, whiteoutMeta):
			// Other .wh..wh. names are aufs bookkeeping, not part of the tree.
		case strings.HasPrefix(base, whiteoutPrefix):
			name := strings.TrimPrefix(base, whiteoutPrefix)
			if name == "" || name == "." || name == ".." {
				return fmt.Errorf("%s: whiteout without a name", rel)
			}
			if err := writeWhiteout(filepath.Join(parent, name)); err != nil {
				return fmt.Errorf("%s: whiteout: %w", rel, err)
			}
		default:
			if err := writeEntry(tr, hdr, root, filepath.Join(parent, base), dirModes); err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
		}
	}
	// Directory modes go last and deepest first, so a directory without
	// write or search permission does not stop the entries below it.
	dirs := make([]string, 0, len(dirModes))
	for d := range dirModes {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(i, j int) bool {
		return strings.Count(dirs[i], "/") > strings.Count(dirs[j], "/")
	})
	for _, d := range dirs {
		if err := syscall.Chmod(d, dirModes[d]); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// writeWhiteout hides target in the lower layers only. An entry of this same
// layer stays visible (image-spec layer.md): a file already covers what is
// below it, and a directory just stops merging with the lower ones.
func writeWhiteout(target string) error {
	fi, err := os.Lstat(target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return syscall.Mknod(target, syscall.S_IFCHR, 0)
	case err != nil:
		return err
	case fi.IsDir():
		return syscall.Setxattr(target, opaqueXattr, []byte("y"), 0)
	}
	return nil
}

// forgetDirs drops the pending modes of p and of everything below it. If p
// comes back as a symlink, a pending chmod below it would follow the link out
// of the layer.
func forgetDirs(dirModes map[string]uint32, p string) {
	delete(dirModes, p)
	for d := range dirModes {
		if strings.HasPrefix(d, p+"/") {
			delete(dirModes, d)
		}
	}
}

func writeEntry(tr io.Reader, hdr *tar.Header, root, p string, dirModes map[string]uint32) error {
	mode := uint32(hdr.Mode) & 0o7777
	fi, err := os.Lstat(p)
	exists := err == nil
	// The only char devices in a layer are whiteouts: real ones become files.
	overWhiteout := exists && fi.Mode()&fs.ModeCharDevice != 0
	if exists && !(fi.IsDir() && hdr.Typeflag == tar.TypeDir) {
		if err := os.RemoveAll(p); err != nil {
			return err
		}
		forgetDirs(dirModes, p)
		exists = false
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		if !exists {
			if err := os.Mkdir(p, 0o755); err != nil {
				return err
			}
		}
		if overWhiteout {
			if err := syscall.Setxattr(p, opaqueXattr, []byte("y"), 0); err != nil {
				return err
			}
		}
		dirModes[p] = mode
	case tar.TypeReg:
		return writeFile(p, tr, mode)
	case tar.TypeChar, tar.TypeBlock:
		// umoci --rootless (v0.6.0) cannot mknod either and writes an empty
		// file with the entry's mode in place of the device.
		return writeFile(p, strings.NewReader(""), mode)
	case tar.TypeSymlink:
		return os.Symlink(hdr.Linkname, p)
	case tar.TypeLink:
		target, err := cleanName(hdr.Linkname)
		if err != nil {
			return err
		}
		if target == "" {
			return fmt.Errorf("hardlink to the layer root")
		}
		if err := walkParents(root, path.Dir(target), false); err != nil {
			return fmt.Errorf("hardlink target %s: %w", target, err)
		}
		return os.Link(filepath.Join(root, filepath.FromSlash(target)), p)
	case tar.TypeFifo:
		if err := syscall.Mkfifo(p, 0o600); err != nil {
			return err
		}
		return syscall.Chmod(p, mode)
	default:
		return fmt.Errorf("tar entry type %q is not supported", hdr.Typeflag)
	}
	return nil
}

func writeFile(p string, content io.Reader, mode uint32) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, content)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return syscall.Chmod(p, mode)
}
