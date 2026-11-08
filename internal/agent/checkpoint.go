package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	andenv1 "github.com/CesarManzoCode/anden/gen/anden/v1"
	ociruntime "github.com/CesarManzoCode/anden/internal/runtime"
	"github.com/CesarManzoCode/anden/internal/wake"
)

// The restore route of D03. The recipe was found by trial without root, and each
// step has its reason: criu resolves the files a process maps by path, inside its
// own mount namespace, and demands the same inode. So the dump runs chrooted in
// the rootfs of the instance, in a private mount namespace where criu and its
// libraries are copied onto a tmpfs. The network, UTS and IPC namespaces stay out
// of the snapshot: the agent keeps them mounted on net.ns, uts.ns and ipc.ns, the
// process that runs criu joins UTS and IPC, and the netns is passed as external.

const (
	routeRestore = "restore"
	// CheckpointCommand is the hidden command of anden-agent that sets up the
	// view of one criu run and execs it.
	CheckpointCommand = "_criu"
	// extnet is the key under which criu knows the netns of the instance.
	extnet = "extnet"
	// netFD is where the restore finds the netns: the first of ExtraFiles.
	netFD = 3
	// viewImages and viewTools are the snapshot and criu inside the dump view.
	viewImages = "/tmp/img"
	viewTools  = "/tmp/c"
	// sysSetns is __NR_setns on x86-64; package syscall does not define it.
	sysSetns = 308
)

// Kinds of step of a mount view.
const (
	opPrivate = "private" // every mount of the namespace becomes private
	opBind    = "bind"
	opTmpfs   = "tmpfs"
	opProc    = "proc"
	opMkdir   = "mkdir" // the directory, if the image lacks it
	opTouch   = "touch" // an empty file to bind onto, if the image lacks it
	opCopy    = "copy"
	opChroot  = "chroot"
)

// step is one change to the mount view of the process that runs criu.
type step struct {
	Op     string `json:"op"`
	Source string `json:"source,omitempty"`
	Target string `json:"target"`
}

// checkpointJob is what the hidden command does: the steps in its own mount
// namespace, then joining UTS and IPC, then exec of Argv.
type checkpointJob struct {
	// Root is the rootfs of the instance. No target under it may go through
	// a symlink: the container wrote those files.
	Root  string   `json:"root"`
	Steps []step   `json:"steps"`
	UTS   string   `json:"uts"`
	IPC   string   `json:"ipc"`
	Argv  []string `json:"argv"`
}

// library is a file the dynamic loader needs: name is how the loader looks for
// it, path where ldd found it.
type library struct {
	name, path string
}

// parseLdd reads `ldd criu`. It returns the files to copy, the interpreter
// among them, and the name of the interpreter. The interpreter line has an
// absolute path on the left; linux-vdso.so.1 has no file.
func parseLdd(out string) ([]library, string, error) {
	var libs []library
	interp := ""
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if strings.Contains(line, "not found") {
			return nil, "", fmt.Errorf("ldd: missing library: %s", strings.TrimSpace(line))
		}
		var name, path string
		switch {
		case len(fields) >= 3 && fields[1] == "=>":
			name, path = fields[0], fields[2]
		case strings.HasPrefix(fields[0], "/"):
			name, path = fields[0], fields[0]
		default:
			continue
		}
		if !strings.HasPrefix(path, "/") {
			continue
		}
		if strings.HasPrefix(name, "/") {
			interp = filepath.Base(name)
		}
		libs = append(libs, library{name: filepath.Base(name), path: path})
	}
	if interp == "" {
		return nil, "", fmt.Errorf("ldd: no dynamic loader in %q", strings.TrimSpace(out))
	}
	return libs, interp, nil
}

// dumpView is the mount view of D03 for the dump of the rootfs merged into the
// directory images, as a list of steps. The mkdir and touch steps only give the
// mounts a target when the image lacks it.
func dumpView(merged, images, criu string, libs []library) []step {
	at := func(rel string) string { return filepath.Join(merged, rel) }
	steps := []step{
		{Op: opPrivate, Target: "/"},
		{Op: opBind, Source: merged, Target: merged},
		{Op: opMkdir, Target: at("tmp")},
		{Op: opTmpfs, Target: at("tmp")},
		{Op: opMkdir, Target: at("proc")},
		{Op: opProc, Target: at("proc")},
		{Op: opMkdir, Target: at("dev")},
		{Op: opTouch, Target: at("dev/null")},
		{Op: opBind, Source: "/dev/null", Target: at("dev/null")},
		{Op: opMkdir, Target: at(viewImages)},
		{Op: opBind, Source: images, Target: at(viewImages)},
		{Op: opMkdir, Target: at(viewTools)},
		{Op: opCopy, Source: criu, Target: at(viewTools + "/criu")},
	}
	for _, lib := range libs {
		steps = append(steps, step{Op: opCopy, Source: lib.path, Target: at(viewTools + "/" + lib.name)})
	}
	return append(steps, step{Op: opChroot, Target: merged})
}

// restoreView is the mount view of the restore: the overlay is already mounted
// on merged, and criu takes it as root.
func restoreView(merged string) []step {
	return []step{
		{Op: opPrivate, Target: "/"},
		{Op: opBind, Source: merged, Target: merged},
	}
}

// deviceMounts are the --external mounts of the device nodes anden-runtime binds
// from the host /dev. criu cannot dump those binds by itself, because the
// container has no mount of the devtmpfs they come from: the dump names each one
// by its mountpoint, mnt[/dev/null]:dev-null, and the restore binds the same
// host node back, mnt[dev-null]:/dev/null.
func deviceMounts(restore bool) []string {
	var args []string
	for _, name := range ociruntime.DefaultDevices {
		path, cookie := "/dev/"+name, "dev-"+name
		if restore {
			args = append(args, "--external", fmt.Sprintf("mnt[%s]:%s", cookie, path))
		} else {
			args = append(args, "--external", fmt.Sprintf("mnt[%s]:%s", path, cookie))
		}
	}
	return args
}

// dumpArgv runs the copied criu through the copied loader, inside the view.
func dumpArgv(interp string, pid int, netInode uint64) []string {
	argv := []string{
		viewTools + "/" + interp, "--library-path", viewTools, viewTools + "/criu",
		"dump", "--network-lock", "skip", "--unprivileged", "--manage-cgroups=ignore",
		"-t", strconv.Itoa(pid), "-D", viewImages, "--root", "/",
		"--external", fmt.Sprintf("net[%d]:%s", netInode, extnet),
	}
	argv = append(argv, deviceMounts(false)...)
	return append(argv, "-o", "dump.log", "-v2")
}

// restoreArgv restores the images onto root, with the netns at netFD.
func restoreArgv(criu, images, root, pidfile string) []string {
	argv := []string{
		criu, "restore", "--network-lock", "skip", "--unprivileged", "--manage-cgroups=ignore",
		"-D", images, "--root", root,
		"--inherit-fd", fmt.Sprintf("fd[%d]:%s", netFD, extnet),
	}
	argv = append(argv, deviceMounts(true)...)
	return append(argv, "-d", "--pidfile", pidfile, "-o", "restore.log", "-v2")
}

// requestedRoute is the wake_route of a spec as the instance records it.
func requestedRoute(spec *andenv1.ServiceSpec) string {
	if spec.GetWakeRoute() == andenv1.WakeRoute_WAKE_ROUTE_RESTORE {
		return routeRestore
	}
	return routeRelaunch
}

// wakeBy wakes by restore when it was asked for and a snapshot exists, and by
// relaunch otherwise or when the restore fails. It returns the route that woke.
func wakeBy(requested string, snapshot bool, restore, relaunch func() error) (string, error) {
	if requested == routeRestore && snapshot {
		err := restore()
		if err == nil {
			return routeRestore, nil
		}
	}
	return routeRelaunch, relaunch()
}

// dump checkpoints the instance into its snapshot directory. criu kills the
// processes once the dump is written; if it fails they go on running.
func (a *Agent) dump(in *instance) error {
	p := in.paths
	if err := os.RemoveAll(p.snapshot); err != nil {
		return err
	}
	if err := os.MkdirAll(p.snapshot, 0o700); err != nil {
		return err
	}
	pid, err := a.containerPid(in)
	if err != nil {
		return err
	}
	var st syscall.Stat_t
	if err := syscall.Stat(p.netNS, &st); err != nil {
		return fmt.Errorf("stat %s: %w", p.netNS, err)
	}
	criu, err := exec.LookPath("criu")
	if err != nil {
		return err
	}
	ldd := exec.Command("ldd", criu)
	ldd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := ldd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ldd %s: %w: %s", criu, err, out)
	}
	libs, interp, err := parseLdd(string(out))
	if err != nil {
		return err
	}
	job := checkpointJob{Root: p.merged, Steps: dumpView(p.merged, p.snapshot, criu, libs), UTS: p.utsNS, IPC: p.ipcNS,
		Argv: dumpArgv(interp, pid, st.Ino)}
	if err := runCheckpoint(job, nil, nil); err != nil {
		return fmt.Errorf("criu dump: %w; see %s", err, filepath.Join(p.snapshot, "dump.log"))
	}
	return nil
}

// restore brings the instance back from its snapshot: the overlay is mounted
// again, and criu runs inside the cgroup of the instance and its kept UTS and
// IPC, with the kept netns as external. The network phase is only opening what
// the sleep kept.
func (a *Agent) restore(in *instance, phases *wake.Phases) error {
	p := in.paths
	if _, err := a.mountImage(in.ctx, in.assignment.GetSpec().GetImageDigest(), p); err != nil {
		return fmt.Errorf("image: %w", err)
	}
	phases.Mark(wake.Rootfs)
	netns, err := os.Open(p.netNS)
	if err != nil {
		return err
	}
	defer netns.Close()
	cgroup, err := os.Open(p.cgroup)
	if err != nil {
		return err
	}
	defer cgroup.Close()
	phases.Mark(wake.Network)
	criu, err := exec.LookPath("criu")
	if err != nil {
		return err
	}
	pidfile := filepath.Join(p.snapshot, "restore.pid")
	if err := os.Remove(pidfile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	job := checkpointJob{Root: p.merged, Steps: restoreView(p.merged), UTS: p.utsNS, IPC: p.ipcNS,
		Argv: restoreArgv(criu, p.snapshot, p.merged, pidfile)}
	if err := runCheckpoint(job, cgroup, netns); err != nil {
		return fmt.Errorf("criu restore: %w; see %s", err, filepath.Join(p.snapshot, "restore.log"))
	}
	raw, err := os.ReadFile(pidfile)
	if err != nil {
		return err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return fmt.Errorf("%s: %w", pidfile, err)
	}
	in.pid = pid
	phases.Mark(wake.Start)
	return nil
}

// containerPid is the root of the process tree of the instance: the one criu
// restored or, after a launch, the one the runtime knows.
func (a *Agent) containerPid(in *instance) (int, error) {
	if in.pid > 0 {
		return in.pid, nil
	}
	out, err := exec.Command(a.cfg.Runtime, a.runtimeArgs("state", in.id)...).Output()
	if err != nil {
		return 0, fmt.Errorf("anden-runtime state: %w", err)
	}
	var state struct {
		Pid int `json:"pid"`
	}
	if err := json.Unmarshal(out, &state); err != nil {
		return 0, err
	}
	if state.Pid <= 0 {
		return 0, fmt.Errorf("anden-runtime state: no pid for %s", in.id)
	}
	return state.Pid, nil
}

// runCheckpoint runs the hidden command of anden-agent in a new mount namespace
// and, with cgroup, born inside that cgroup, so that what criu restores is too.
func runCheckpoint(job checkpointJob, cgroup *os.File, netns *os.File) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(job)
	if err != nil {
		return err
	}
	cmd := exec.Command(self, CheckpointCommand, string(raw))
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNS}
	if cgroup != nil {
		cmd.SysProcAttr.UseCgroupFD = true
		cmd.SysProcAttr.CgroupFD = int(cgroup.Fd())
	}
	if netns != nil {
		cmd.ExtraFiles = []*os.File{netns}
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RunCheckpoint is the hidden command: it applies the steps of the job in the
// mount namespace it was born in, joins the UTS and IPC of the instance and
// execs criu. It only returns on failure.
func RunCheckpoint(arg string) error {
	var job checkpointJob
	if err := json.Unmarshal([]byte(arg), &job); err != nil {
		return err
	}
	if len(job.Argv) == 0 {
		return errors.New("no command to run")
	}
	// setns changes the namespaces of one thread, and exec keeps the ones of the
	// thread that calls it.
	runtime.LockOSThread()
	// Opened before any chroot hides them.
	uts, err := os.Open(job.UTS)
	if err != nil {
		return err
	}
	ipc, err := os.Open(job.IPC)
	if err != nil {
		return err
	}
	for _, s := range job.Steps {
		if err := applyStep(job.Root, s); err != nil {
			return fmt.Errorf("%s %s: %w", s.Op, s.Target, err)
		}
	}
	if _, _, e := syscall.RawSyscall(sysSetns, uts.Fd(), syscall.CLONE_NEWUTS, 0); e != 0 {
		return fmt.Errorf("setns uts: %w", e)
	}
	if _, _, e := syscall.RawSyscall(sysSetns, ipc.Fd(), syscall.CLONE_NEWIPC, 0); e != 0 {
		return fmt.Errorf("setns ipc: %w", e)
	}
	uts.Close()
	ipc.Close()
	return syscall.Exec(job.Argv[0], job.Argv, os.Environ())
}

func applyStep(root string, s step) error {
	if s.Op != opPrivate && s.Op != opChroot {
		if err := noSymlinks(root, s.Target); err != nil {
			return err
		}
	}
	switch s.Op {
	case opPrivate:
		return syscall.Mount("", s.Target, "", syscall.MS_REC|syscall.MS_PRIVATE, "")
	case opBind:
		return syscall.Mount(s.Source, s.Target, "", syscall.MS_BIND, "")
	case opTmpfs:
		return syscall.Mount("tmpfs", s.Target, "tmpfs", 0, "mode=755")
	case opProc:
		return syscall.Mount("proc", s.Target, "proc", 0, "")
	case opMkdir:
		return makeDir(s.Target)
	case opTouch:
		return touch(s.Target)
	case opCopy:
		return copyFile(s.Source, s.Target)
	case opChroot:
		if err := syscall.Chroot(s.Target); err != nil {
			return err
		}
		return syscall.Chdir("/")
	}
	return fmt.Errorf("unknown step %q", s.Op)
}

// noSymlinks refuses a target under root that goes through a symlink: it would
// lead the mount out of the rootfs.
func noSymlinks(root, target string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return nil
	}
	path := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		path = filepath.Join(path, part)
		fi, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink", path)
		}
	}
	return nil
}

// touch creates an empty file if nothing is there.
func touch(path string) error {
	fi, err := os.Lstat(path)
	if err == nil {
		if fi.IsDir() {
			return fmt.Errorf("%s is a directory", path)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// sleepByDump is the sleep of the restore route: it reports whether a snapshot
// was left. Without one the sleep goes on the normal way.
func (a *Agent) sleepByDump(in *instance) bool {
	if err := a.dump(in); err != nil {
		log.Printf("instance %s: no snapshot, sleeping without one: %v", in.id, err)
		return false
	}
	if !waitEmpty(in.paths.cgroup, killTimeout) {
		log.Printf("instance %s: processes left after the dump", in.id)
	}
	return true
}
