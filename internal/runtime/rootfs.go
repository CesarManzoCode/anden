package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type mountOptions struct {
	Flags       uintptr
	Propagation []uintptr
	Data        string
}

var mountFlagSet = map[string]uintptr{
	"ro": syscall.MS_RDONLY, "nosuid": syscall.MS_NOSUID, "nodev": syscall.MS_NODEV,
	"noexec": syscall.MS_NOEXEC, "sync": syscall.MS_SYNCHRONOUS, "dirsync": syscall.MS_DIRSYNC,
	"remount": syscall.MS_REMOUNT, "mand": syscall.MS_MANDLOCK, "noatime": syscall.MS_NOATIME,
	"nodiratime": syscall.MS_NODIRATIME, "bind": syscall.MS_BIND,
	"rbind": syscall.MS_BIND | syscall.MS_REC, "relatime": syscall.MS_RELATIME,
	"strictatime": syscall.MS_STRICTATIME, "silent": syscall.MS_SILENT,
}

var mountFlagClear = map[string]uintptr{
	"rw": syscall.MS_RDONLY, "suid": syscall.MS_NOSUID, "dev": syscall.MS_NODEV,
	"exec": syscall.MS_NOEXEC, "async": syscall.MS_SYNCHRONOUS, "nomand": syscall.MS_MANDLOCK,
	"atime": syscall.MS_NOATIME, "diratime": syscall.MS_NODIRATIME,
	"norelatime": syscall.MS_RELATIME, "nostrictatime": syscall.MS_STRICTATIME,
	"loud": syscall.MS_SILENT, "defaults": 0,
}

var propagationFlags = map[string]uintptr{
	"private": syscall.MS_PRIVATE, "rprivate": syscall.MS_PRIVATE | syscall.MS_REC,
	"shared": syscall.MS_SHARED, "rshared": syscall.MS_SHARED | syscall.MS_REC,
	"slave": syscall.MS_SLAVE, "rslave": syscall.MS_SLAVE | syscall.MS_REC,
	"unbindable": syscall.MS_UNBINDABLE, "runbindable": syscall.MS_UNBINDABLE | syscall.MS_REC,
}

// Options that need mount_setattr or a copy step; rejected instead of
// being passed to the filesystem as data.
var unsupportedMountOptions = map[string]bool{
	"idmap": true, "ridmap": true, "tmpcopyup": true,
	"rro": true, "rrw": true, "rnosuid": true, "rsuid": true, "rnodev": true, "rdev": true,
	"rnoexec": true, "rexec": true, "rnoatime": true, "ratime": true, "rrelatime": true,
	"rnorelatime": true, "rstrictatime": true, "rnostrictatime": true, "rnodiratime": true,
	"rdiratime": true, "rnosymfollow": true, "rsymfollow": true,
}

func parseMountOptions(options []string) (mountOptions, error) {
	var o mountOptions
	var data []string
	for _, opt := range options {
		if unsupportedMountOptions[opt] {
			return o, fmt.Errorf("mount option %q is not supported", opt)
		}
		if f, ok := mountFlagSet[opt]; ok {
			o.Flags |= f
		} else if f, ok := mountFlagClear[opt]; ok {
			o.Flags &^= f
		} else if f, ok := propagationFlags[opt]; ok {
			o.Propagation = append(o.Propagation, f)
		} else {
			data = append(data, opt)
		}
	}
	o.Data = strings.Join(data, ",")
	return o, nil
}

// securePath resolves path inside root as if root were "/": symlinks and
// ".." never leave it. root may be relative (init works from ".").
func securePath(root, path string) (string, error) {
	current := "/"
	rest := path
	links := 0
	for rest != "" {
		var part string
		part, rest, _ = strings.Cut(rest, "/")
		switch part {
		case "", ".":
			continue
		case "..":
			current = filepath.Dir(current)
			continue
		}
		next := filepath.Join(current, part)
		fi, err := os.Lstat(filepath.Join(root, next))
		if errors.Is(err, os.ErrNotExist) {
			return filepath.Join(root, filepath.Join("/", next, rest)), nil
		}
		if err != nil {
			return "", err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			current = next
			continue
		}
		links++
		if links > 255 {
			return "", fmt.Errorf("%s: too many symlinks", path)
		}
		target, err := os.Readlink(filepath.Join(root, next))
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(target) {
			current = "/"
		}
		rest = target + "/" + rest
	}
	return filepath.Join(root, current), nil
}

// lockedFlags returns the flags a remount must keep: in a user namespace
// the kernel refuses to clear nosuid, nodev, noexec or the atime mode of a
// mount inherited from a more privileged one.
func lockedFlags(path string) (uintptr, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	const (
		stNosuid     = 0x2
		stNodev      = 0x4
		stNoexec     = 0x8
		stNoatime    = 0x400
		stNodiratime = 0x800
		stRelatime   = 0x1000
	)
	var flags uintptr
	for bit, ms := range map[int64]uintptr{
		stNosuid: syscall.MS_NOSUID, stNodev: syscall.MS_NODEV, stNoexec: syscall.MS_NOEXEC,
		stNoatime: syscall.MS_NOATIME, stNodiratime: syscall.MS_NODIRATIME, stRelatime: syscall.MS_RELATIME,
	} {
		if st.Flags&bit != 0 {
			flags |= ms
		}
	}
	return flags, nil
}

var deviceModes = map[string]uint32{
	"c": syscall.S_IFCHR, "u": syscall.S_IFCHR, "b": syscall.S_IFBLK, "p": syscall.S_IFIFO,
}

// DefaultDevices are the nodes every container gets on its own /dev, bound
// from the host /dev.
var DefaultDevices = []string{"null", "zero", "full", "random", "urandom", "tty"}

// setupRootfs builds the container filesystem inside init's mount
// namespace and pivots into it. Every path after the first bind is
// relative to the rootfs, which is the working directory: in a new user
// namespace the bundle path may no longer be reachable once init becomes
// root there.
func setupRootfs(c *initConfig) error {
	spec := c.Spec
	pre := uintptr(syscall.MS_SLAVE | syscall.MS_REC)
	if p := spec.Linux.RootfsPropagation; p == "private" || p == "rprivate" {
		pre = syscall.MS_PRIVATE | syscall.MS_REC
	}
	if err := syscall.Mount("", "/", "", pre, ""); err != nil {
		return fmt.Errorf("set propagation of /: %w", err)
	}
	if err := syscall.Mount(c.Rootfs, c.Rootfs, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("bind rootfs %s: %w", c.Rootfs, err)
	}
	rootfd, err := syscall.Open(c.Rootfs, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open rootfs %s: %w", c.Rootfs, err)
	}
	defer syscall.Close(rootfd)
	if c.NewUserNS {
		if err := setIDs(User{}); err != nil {
			return fmt.Errorf("become root of the new user namespace: %w", err)
		}
	}
	if err := syscall.Fchdir(rootfd); err != nil {
		return fmt.Errorf("enter rootfs: %w", err)
	}

	devBound := false
	for _, m := range spec.Mounts {
		if err := mountEntry(m, c.Bundle); err != nil {
			return fmt.Errorf("mount %s: %w", m.Destination, err)
		}
		o, _ := parseMountOptions(m.Options)
		if filepath.Clean(m.Destination) == "/dev" && (m.Type == "bind" || o.Flags&syscall.MS_BIND != 0) {
			devBound = true
		}
	}
	for _, d := range spec.Linux.Devices {
		if err := createDevice(d); err != nil {
			return fmt.Errorf("device %s: %w", d.Path, err)
		}
	}
	if !devBound {
		if err := setupDev(); err != nil {
			return err
		}
	}
	return pivotRoot()
}

func mountEntry(m Mount, bundle string) error {
	o, err := parseMountOptions(m.Options)
	if err != nil {
		return err
	}
	dest, err := securePath(".", m.Destination)
	if err != nil {
		return err
	}
	if m.Type == "bind" || o.Flags&syscall.MS_BIND != 0 {
		src := m.Source
		if !filepath.IsAbs(src) {
			src = filepath.Join(bundle, src)
		}
		fi, err := os.Stat(src)
		if err != nil {
			return err
		}
		if err := makeMountpoint(dest, fi.IsDir()); err != nil {
			return err
		}
		if err := syscall.Mount(src, dest, "", syscall.MS_BIND|(o.Flags&syscall.MS_REC), ""); err != nil {
			return err
		}
		if extra := o.Flags &^ (syscall.MS_BIND | syscall.MS_REC); extra != 0 {
			if err := remountBind(dest, extra); err != nil {
				return err
			}
		}
	} else {
		if err := makeMountpoint(dest, true); err != nil {
			return err
		}
		err := syscall.Mount(m.Source, dest, m.Type, o.Flags, o.Data)
		if errors.Is(err, syscall.EPERM) && m.Type == "sysfs" {
			// sysfs can only be mounted for a network namespace owned by
			// the current user namespace; otherwise bind the visible one.
			err = syscall.Mount("/sys", dest, "", syscall.MS_BIND|syscall.MS_REC, "")
			if err == nil {
				err = remountBind(dest, o.Flags)
			}
		}
		if err != nil {
			return err
		}
	}
	for _, p := range o.Propagation {
		if err := syscall.Mount("", dest, "", p, ""); err != nil {
			return fmt.Errorf("set propagation: %w", err)
		}
	}
	return nil
}

// remountBind applies flags to an existing bind mount, keeping the ones
// the kernel does not let a user namespace clear.
func remountBind(path string, flags uintptr) error {
	locked, err := lockedFlags(path)
	if err != nil {
		return err
	}
	flags &^= syscall.MS_BIND | syscall.MS_REC | syscall.MS_REMOUNT
	if err := syscall.Mount("", path, "", syscall.MS_BIND|syscall.MS_REMOUNT|flags|locked, ""); err != nil {
		return fmt.Errorf("remount %s: %w", path, err)
	}
	return nil
}

func makeMountpoint(path string, dir bool) error {
	if dir {
		return os.MkdirAll(path, 0o755)
	}
	if _, err := os.Lstat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_CLOEXEC, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

func mkdev(major, minor int64) uint64 {
	ma, mi := uint64(major), uint64(minor)
	return (mi & 0xff) | ((ma & 0xfff) << 8) | ((mi &^ 0xff) << 12) | ((ma &^ 0xfff) << 32)
}

// createDevice makes a linux.devices node. A user namespace cannot mknod,
// so then the node of the same path on the host is bound instead.
func createDevice(d Device) error {
	dest, err := securePath(".", d.Path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if err := os.Remove(dest); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	perm := uint32(0o666)
	if d.FileMode != nil {
		perm = *d.FileMode & 0o7777
	}
	err = syscall.Mknod(dest, deviceModes[d.Type]|perm, int(mkdev(d.Major, d.Minor)))
	if errors.Is(err, syscall.EPERM) && d.Type != "p" {
		return bindDevice(d.Path, dest)
	}
	if err != nil {
		return err
	}
	if d.UID != nil || d.GID != nil {
		uid, gid := -1, -1
		if d.UID != nil {
			uid = int(*d.UID)
		}
		if d.GID != nil {
			gid = int(*d.GID)
		}
		return os.Lchown(dest, uid, gid)
	}
	return nil
}

func bindDevice(host, dest string) error {
	if err := makeMountpoint(dest, false); err != nil {
		return err
	}
	if err := syscall.Mount(host, dest, "", syscall.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind %s: %w", host, err)
	}
	return nil
}

// setupDev fills the container /dev with the default nodes and symlinks.
func setupDev() error {
	dev, err := securePath(".", "/dev")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dev, 0o755); err != nil {
		return err
	}
	for _, name := range DefaultDevices {
		dest := filepath.Join(dev, name)
		// An empty regular file is the mountpoint an earlier run left in a
		// /dev that is part of the rootfs: it is bound again.
		if fi, err := os.Lstat(dest); err == nil && !(fi.Mode().IsRegular() && fi.Size() == 0) {
			continue
		}
		if err := bindDevice("/dev/"+name, dest); err != nil {
			return fmt.Errorf("device /dev/%s: %w", name, err)
		}
	}
	links := [][2]string{
		{"/proc/self/fd", "fd"}, {"/proc/self/fd/0", "stdin"},
		{"/proc/self/fd/1", "stdout"}, {"/proc/self/fd/2", "stderr"},
	}
	if fi, err := os.Stat(filepath.Join(dev, "pts")); err == nil && fi.IsDir() {
		if err := os.Remove(filepath.Join(dev, "ptmx")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		links = append(links, [2]string{"pts/ptmx", "ptmx"})
	}
	for _, l := range links {
		path := filepath.Join(dev, l[1])
		if _, err := os.Lstat(path); err == nil {
			continue
		}
		if err := os.Symlink(l[0], path); err != nil {
			return fmt.Errorf("symlink /dev/%s: %w", l[1], err)
		}
	}
	return nil
}

// pivotRoot makes the working directory the new root with
// pivot_root(".", ".") and detaches the old root stacked on top of it.
func pivotRoot() error {
	oldroot, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(oldroot)
	if err := syscall.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := syscall.Fchdir(oldroot); err != nil {
		return err
	}
	// Keep the unmount of the old root from propagating to the host.
	if err := syscall.Mount("", ".", "", syscall.MS_SLAVE|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("make old root slave: %w", err)
	}
	if err := syscall.Unmount(".", syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("detach old root: %w", err)
	}
	return syscall.Chdir("/")
}

// finalizeRootfs runs inside the new root: sysctl, readonly and masked
// paths, a read-only root and the requested root propagation, in that
// order (sysctl writes /proc/sys before it can become read-only).
func finalizeRootfs(spec *Spec) error {
	l := spec.Linux
	for key, value := range l.Sysctl {
		path := "/proc/sys/" + strings.ReplaceAll(key, ".", "/")
		if err := os.WriteFile(path, []byte(value), 0); err != nil {
			return fmt.Errorf("sysctl %s: %w", key, err)
		}
	}
	for _, p := range l.ReadonlyPaths {
		err := syscall.Mount(p, p, "", syscall.MS_BIND|syscall.MS_REC, "")
		if errors.Is(err, syscall.ENOENT) {
			continue
		}
		if err == nil {
			err = remountBind(p, syscall.MS_RDONLY)
		}
		if err != nil {
			return fmt.Errorf("readonly path %s: %w", p, err)
		}
	}
	for _, p := range l.MaskedPaths {
		fi, err := os.Stat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("masked path %s: %w", p, err)
		}
		if fi.IsDir() {
			err = syscall.Mount("tmpfs", p, "tmpfs", syscall.MS_RDONLY, "")
		} else {
			err = syscall.Mount("/dev/null", p, "", syscall.MS_BIND, "")
		}
		if err != nil {
			return fmt.Errorf("masked path %s: %w", p, err)
		}
	}
	if spec.Root.Readonly {
		if err := remountBind("/", syscall.MS_RDONLY); err != nil {
			return fmt.Errorf("read-only root: %w", err)
		}
	}
	if p := l.RootfsPropagation; p != "" {
		if err := syscall.Mount("", "/", "", propagationFlags[p], ""); err != nil {
			return fmt.Errorf("rootfs propagation %s: %w", p, err)
		}
	}
	return nil
}
