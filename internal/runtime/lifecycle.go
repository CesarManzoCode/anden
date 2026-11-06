package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"syscall"
	"time"
)

// create re-executes this binary as init inside the namespaces of
// config.json and returns once init has built the rootfs and waits on the
// FIFO for start.
func create(s store, id, bundle, pidFile string) (err error) {
	bundle, err = filepath.Abs(bundle)
	if err != nil {
		return err
	}
	spec, err := LoadSpec(bundle)
	if err != nil {
		return err
	}
	rootfs := spec.Root.Path
	if !filepath.IsAbs(rootfs) {
		rootfs = filepath.Join(bundle, rootfs)
	}
	if rootfs, err = filepath.EvalSymlinks(rootfs); err != nil {
		return fmt.Errorf("root.path: %w", err)
	}
	if fi, err := os.Stat(rootfs); err != nil || !fi.IsDir() {
		return fmt.Errorf("root.path %s is not a directory", rootfs)
	}

	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return err
	}
	dir := s.dir(id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("container %s already exists", id)
		}
		return err
	}
	rec := &record{ID: id, Bundle: bundle, Annotations: spec.Annotations}
	var cmd *exec.Cmd
	defer func() {
		if err == nil {
			return
		}
		if cmd != nil && cmd.Process != nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
		if rec.CgroupOwned {
			removeCgroup(rec.Cgroup)
		}
		os.RemoveAll(dir)
	}()
	if err := s.save(rec); err != nil {
		return err
	}

	plan := planNamespaces(spec.Linux)
	ic := initConfig{Spec: spec, Rootfs: rootfs, Bundle: bundle, NewUserNS: plan.NewUser, CapLast: lastCapability()}

	configR, configW, err := os.Pipe()
	if err != nil {
		return err
	}
	defer configW.Close()
	syncR, syncW, err := os.Pipe()
	if err != nil {
		configR.Close()
		return err
	}
	defer syncR.Close()
	fifoPath := filepath.Join(dir, fifoFile)
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		return fmt.Errorf("mkfifo: %w", err)
	}
	// O_RDWR opens a FIFO without waiting for the other end; init keeps
	// this descriptor and blocks reading it until start writes one byte.
	fifo, err := os.OpenFile(fifoPath, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	extra := []*os.File{configR, syncW, fifo}
	defer func() {
		for _, f := range extra {
			f.Close()
		}
	}()
	for _, ns := range plan.JoinPaths {
		f, err := openNamespace(ns)
		if err != nil {
			return err
		}
		ic.Joins = append(ic.Joins, nsJoin{Type: ns.Type, FD: 3 + len(extra)})
		extra = append(extra, f)
	}
	var pidns *os.File
	if plan.PidPath != "" {
		if pidns, err = openNamespace(Namespace{Type: "pid", Path: plan.PidPath}); err != nil {
			return err
		}
		defer pidns.Close()
	}

	selfCgroup, _ := os.ReadFile("/proc/self/cgroup")
	cg, err := planCgroup(spec.Linux, id, selfCgroup)
	if err != nil {
		return err
	}
	attr := &syscall.SysProcAttr{Cloneflags: plan.CloneFlags}
	if cg != nil {
		owned, err := setupCgroup(cg)
		if err != nil {
			return err
		}
		rec.Cgroup, rec.CgroupOwned = cg.Dir, owned
		if err := s.save(rec); err != nil {
			return err
		}
		cgfd, err := os.Open(cg.Dir)
		if err != nil {
			return err
		}
		defer cgfd.Close()
		// clone3 with CLONE_INTO_CGROUP: init never runs outside it.
		attr.UseCgroupFD, attr.CgroupFD = true, int(cgfd.Fd())
	}
	if plan.NewUser {
		attr.UidMappings = idMaps(spec.Linux.UIDMappings)
		attr.GidMappings = idMaps(spec.Linux.GIDMappings)
		attr.GidMappingsEnableSetgroups = true
		// Until init becomes root of the new namespace it keeps the
		// caller's uid there, unmapped; ambient capabilities keep its
		// capabilities across the exec of init.
		for c := 0; c <= ic.CapLast; c++ {
			attr.AmbientCaps = append(attr.AmbientCaps, uintptr(c))
		}
	}

	cmd = &exec.Cmd{Path: "/proc/self/exe", Args: []string{"anden-runtime", "init"}, ExtraFiles: extra, SysProcAttr: attr}
	if f := stdFile(0, os.Stdin); f != nil {
		cmd.Stdin = f
	}
	if f := stdFile(1, os.Stdout); f != nil {
		cmd.Stdout = f
	}
	if f := stdFile(2, os.Stderr); f != nil {
		cmd.Stderr = f
	}
	started := make(chan error, 1)
	go func() {
		// Never unlocked: a thread that joined another PID namespace
		// must end with this goroutine.
		goruntime.LockOSThread()
		if pidns != nil {
			if err := setns(int(pidns.Fd()), "pid"); err != nil {
				started <- err
				return
			}
		}
		started <- cmd.Start()
	}()
	if err := <-started; err != nil {
		cmd = nil
		return fmt.Errorf("start init: %w", err)
	}
	for _, f := range extra {
		f.Close()
	}
	extra = nil

	data, err := json.Marshal(ic)
	if err != nil {
		return err
	}
	_, werr := configW.Write(data)
	configW.Close()
	msg, rerr := io.ReadAll(syncR)
	var reply initReply
	if len(msg) == 0 || json.Unmarshal(msg, &reply) != nil {
		return fmt.Errorf("init exited before reporting (write: %v, read: %v)", werr, rerr)
	}
	if reply.Error != "" {
		return errors.New(reply.Error)
	}

	pid := cmd.Process.Pid
	st, err := readProcStat(pid)
	if err != nil {
		return fmt.Errorf("init %d: %w", pid, err)
	}
	rec.Pid, rec.StartTime = pid, st.StartTime
	if err := s.save(rec); err != nil {
		return err
	}
	if pidFile != "" {
		if err := writeFileAtomic(pidFile, []byte(strconv.Itoa(pid))); err != nil {
			return fmt.Errorf("pid file: %w", err)
		}
	}
	cmd.Process.Release()
	return nil
}

// stdFile returns a standard stream to hand to init unchanged, or nil if
// the descriptor is closed.
func stdFile(fd int, f *os.File) *os.File {
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil {
		return nil
	}
	return f
}

func idMaps(m []IDMapping) []syscall.SysProcIDMap {
	out := make([]syscall.SysProcIDMap, len(m))
	for i, e := range m {
		out[i] = syscall.SysProcIDMap{ContainerID: int(e.ContainerID), HostID: int(e.HostID), Size: int(e.Size)}
	}
	return out
}

// start lets init exec the user process by writing one byte to the FIFO,
// then removes the FIFO: from then on the container reads as running.
func start(s store, id string) error {
	r, err := s.load(id)
	if err != nil {
		return err
	}
	if st := s.status(r); !canStart(st) {
		return fmt.Errorf("container %s is %s, not created", id, st)
	}
	path := filepath.Join(s.dir(id), fifoFile)
	fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if errors.Is(err, syscall.ENXIO) {
		return fmt.Errorf("container %s is not waiting for start", id)
	}
	if err != nil {
		return err
	}
	_, err = syscall.Write(fd, []byte{0})
	syscall.Close(fd)
	if err != nil {
		return fmt.Errorf("signal start: %w", err)
	}
	return os.Remove(path)
}

func kill(s store, id string, sig syscall.Signal) error {
	r, err := s.load(id)
	if err != nil {
		return err
	}
	if st := s.status(r); !canKill(st) {
		return fmt.Errorf("container %s is %s; only created or running containers take signals", id, st)
	}
	return syscall.Kill(r.Pid, sig)
}

// remove deletes a created or stopped container. A created one is killed
// first; with force a running one is too.
func remove(s store, id string, force bool) error {
	r, err := s.load(id)
	if err != nil {
		return err
	}
	if st := s.status(r); !canDelete(st, force) {
		return fmt.Errorf("container %s is %s; delete needs created or stopped, or --force", id, st)
	}
	if r.Pid > 0 && processAlive(r.Pid, r.StartTime) {
		if err := syscall.Kill(r.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		deadline := time.Now().Add(5 * time.Second)
		for processAlive(r.Pid, r.StartTime) {
			if time.Now().After(deadline) {
				return fmt.Errorf("container %s: process %d did not exit after SIGKILL", id, r.Pid)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if r.CgroupOwned {
		if err := removeCgroup(r.Cgroup); err != nil {
			return err
		}
	}
	return os.RemoveAll(s.dir(id))
}
