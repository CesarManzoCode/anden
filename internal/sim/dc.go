package sim

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// hostProcEnv names the inherited descriptor of the host's /proc, through
// which _dc-init translates its pids to the host pid namespace.
const hostProcEnv = "ANDEN_HOST_PROC_FD"

// holdSeconds keeps a holder asleep for good; sleep(1) documents integers,
// not "infinity".
const holdSeconds = "2147483647"

// RunDC is `anden _dc`, the first process of the scope. It leaves the scope
// root for sup/, enables the controllers, starts Postgres in pg/ and runs
// _dc-init in the datacenter namespace, in dc/hold/.
func RunDC(state, sockets string, log io.Writer) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return err
	}
	scope, err := cgroupDir(raw)
	if err != nil {
		return err
	}
	dc := filepath.Join(scope, "dc")
	for _, d := range []string{"sup", "pg", "dc"} {
		if err := mkdirCgroup(filepath.Join(scope, d)); err != nil {
			return err
		}
	}
	// No internal processes: the scope root must be empty before it
	// delegates controllers to its children.
	if err := writeFile(filepath.Join(scope, "sup", "cgroup.procs"), strconv.Itoa(os.Getpid())); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(scope, "cgroup.subtree_control"), Controllers); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dc, "cgroup.subtree_control"), Controllers); err != nil {
		return err
	}
	if err := mkdirCgroup(filepath.Join(dc, "hold")); err != nil {
		return err
	}
	if err := os.MkdirAll(sockets, 0o700); err != nil {
		return err
	}
	if err := startPostgres(state, sockets, filepath.Join(scope, "pg"), log); err != nil {
		return fmt.Errorf("postgres: %w", err)
	}

	hostProc, err := os.Open("/proc")
	if err != nil {
		return err
	}
	defer hostProc.Close()
	hold, err := os.Open(filepath.Join(dc, "hold"))
	if err != nil {
		return err
	}
	defer hold.Close()
	cmd := exec.Command("unshare", "-U", "--map-root-user", "--map-auto", "--pid", "--fork", "--kill-child",
		"--net", "--mount", "--mount-proc", "--", self, "_dc-init", "--state", state, "--sockets", sockets)
	cmd.ExtraFiles = []*os.File{hostProc}
	cmd.Env = append(os.Environ(), hostProcEnv+"=3")
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(hold.Fd())}
	return cmd.Run()
}

// startPostgres initializes the cluster if it does not exist yet and starts
// it with pg_ctl in the pg/ cgroup, listening only on the Unix socket.
func startPostgres(state, sockets, cgroup string, log io.Writer) error {
	data := filepath.Join(state, "pg")
	if _, err := os.Stat(filepath.Join(data, "PG_VERSION")); err != nil {
		initdb := exec.Command("initdb", "-D", data, "-A", "trust", "-U", "postgres", "-N", "-E", "UTF8", "--no-instructions")
		initdb.Stdout, initdb.Stderr = log, log
		if err := initdb.Run(); err != nil {
			return fmt.Errorf("initdb: %w", err)
		}
	}
	cg, err := os.Open(cgroup)
	if err != nil {
		return err
	}
	defer cg.Close()
	ctl := exec.Command("pg_ctl", "-D", data, "-w", "-l", filepath.Join(state, "pg.log"),
		"-o", "-k "+sockets+" -c listen_addresses=''", "start")
	ctl.Stdout, ctl.Stderr = log, log
	ctl.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(cg.Fd())}
	if err := ctl.Run(); err != nil {
		return fmt.Errorf("pg_ctl start: %w", err)
	}
	return nil
}

// RunDCInit is `anden _dc-init`, PID 1 of the datacenter namespace. It sits
// in dc/hold/, because it holds the root netns and dc/ delegates
// controllers. It builds the network, starts andend and the agents, writes
// dc.json and then reaps orphans for as long as the datacenter lives.
func RunDCInit(state, sockets string, log io.Writer) error {
	fd := os.Getenv(hostProcEnv)
	if fd == "" {
		return fmt.Errorf("%s is not set: _dc-init runs only under anden _dc", hostProcEnv)
	}
	hostProc := "/proc/self/fd/" + fd
	self, err := os.Executable()
	if err != nil {
		return err
	}
	// bin/ stays reachable: everything here runs as root of the namespace,
	// which is the owner of the checkout, so it is not copied.
	binDir := filepath.Dir(self)
	for _, b := range []string{"andend", "anden-agent"} {
		if _, err := os.Stat(filepath.Join(binDir, b)); err != nil {
			return fmt.Errorf("%s must sit next to anden: %w", b, err)
		}
	}
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return err
	}
	hold, err := cgroupDir(raw)
	if err != nil {
		return err
	}
	dcCg := filepath.Dir(hold)
	scope := filepath.Dir(dcCg)

	if err := mkdirCgroup(filepath.Join(dcCg, "cp")); err != nil {
		return err
	}
	for _, n := range Nodes() {
		dir := filepath.Join(dcCg, n.Name)
		if err := mkdirCgroup(dir); err != nil {
			return err
		}
		if err := writeFile(filepath.Join(dir, "cgroup.subtree_control"), Controllers); err != nil {
			return err
		}
		if err := mkdirCgroup(filepath.Join(dir, "agent")); err != nil {
			return err
		}
	}

	sleep, err := exec.LookPath("sleep")
	if err != nil {
		return err
	}
	holders := map[string]int{}
	for _, peer := range peers() {
		flags := uintptr(syscall.CLONE_NEWNET)
		if _, isNode := FindNode(peer); isNode {
			flags |= syscall.CLONE_NEWNS
		}
		cmd := &exec.Cmd{Path: sleep, Args: []string{HolderArgv0(peer), holdSeconds},
			SysProcAttr: &syscall.SysProcAttr{Cloneflags: flags}}
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("holder of %s: %w", peer, err)
		}
		holders[peer] = cmd.Process.Pid
	}

	if err := ipBatch(nil, RootLinkBatch(holders), log); err != nil {
		return err
	}
	for _, peer := range peers() {
		lines, err := PeerBatch(peer)
		if err != nil {
			return err
		}
		ns := []string{"-t", strconv.Itoa(holders[peer]), "-n"}
		if err := ipBatch(ns, lines, log); err != nil {
			return fmt.Errorf("%s: %w", peer, err)
		}
		if _, isNode := FindNode(peer); isNode {
			if err := run(log, "nsenter", append(ns, "--", "sysctl", "-q", "-w", "net.ipv4.ip_forward=1")...); err != nil {
				return err
			}
		}
	}
	if err := writeFile("/proc/sys/net/ipv4/ip_forward", "1"); err != nil {
		return err
	}
	if err := ipBatch(nil, RootRouteBatch(), log); err != nil {
		return err
	}
	for _, n := range Nodes() {
		disk := NodeDisk(state, n.Name)
		if err := os.MkdirAll(disk, 0o755); err != nil {
			return err
		}
		if err := run(log, "nsenter", "-t", strconv.Itoa(holders[n.Name]), "-m", "--", "mount", "--bind", disk, disk); err != nil {
			return err
		}
	}

	andend, err := startIn(filepath.Join(dcCg, "cp"), filepath.Join(state, "andend.log"),
		filepath.Join(binDir, "andend"), AndendArgs(sockets)...)
	if err != nil {
		return fmt.Errorf("andend: %w", err)
	}
	for _, n := range Nodes() {
		args := append([]string{"-t", strconv.Itoa(holders[n.Name]), "-n", "-m", "--", filepath.Join(binDir, "anden-agent")},
			AgentArgs(n, state, dcCg, binDir)...)
		if _, err := startIn(filepath.Join(dcCg, n.Name, "agent"), filepath.Join(NodeDisk(state, n.Name), "agent.log"),
			"nsenter", args...); err != nil {
			return fmt.Errorf("agent of %s: %w", n.Name, err)
		}
	}
	if err := waitSocket(APISocket(sockets), andend, 60*time.Second); err != nil {
		return err
	}

	inner := []int{}
	for _, pid := range holders {
		inner = append(inner, pid)
	}
	selfHost, hostOf, err := hostPIDs(hostProc, inner)
	if err != nil {
		return fmt.Errorf("host pids: %w", err)
	}
	ns := map[string]int{RootName: selfHost}
	for peer, pid := range holders {
		host, ok := hostOf[pid]
		if !ok {
			return fmt.Errorf("no host pid for the holder of %s (pid %d here)", peer, pid)
		}
		ns[peer] = host
	}
	dc := DC{ScopeCgroup: scope, DCPid: selfHost, NS: ns, PGSocketDir: sockets, APISocket: APISocket(sockets), EdgeIP: EdgeIP}
	if err := writeDC(state, &dc); err != nil {
		return err
	}
	fmt.Fprintln(log, "anden _dc-init: datacenter up")
	reap()
	return nil
}

// startIn starts a long-lived process directly inside a cgroup, with its
// output in logPath. It is never waited for: reap collects it.
func startIn(cgroup, logPath, path string, args ...string) (*os.Process, error) {
	cg, err := os.Open(cgroup)
	if err != nil {
		return nil, err
	}
	defer cg.Close()
	out, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	cmd := exec.Command(path, args...)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(cg.Fd())}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd.Process, nil
}

// waitSocket waits for andend's socket, failing early if andend exits.
func waitSocket(path string, p *os.Process, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		var ws syscall.WaitStatus
		if pid, _ := syscall.Wait4(p.Pid, &ws, syscall.WNOHANG, nil); pid == p.Pid {
			return fmt.Errorf("andend exited (%v) before creating %s; see andend.log", ws, path)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("andend did not create %s in %s", path, timeout)
}

// hostPIDs returns the host pid of this process and of its children whose
// pids here are inner, read from the host's /proc: its "self" link names
// this process in the host namespace, task/*/children lists the children
// and their NSpid ends with the pid seen here.
func hostPIDs(hostProc string, inner []int) (int, map[int]int, error) {
	link, err := os.Readlink(filepath.Join(hostProc, "self"))
	if err != nil {
		return 0, nil, err
	}
	self, err := strconv.Atoi(link)
	if err != nil {
		return 0, nil, fmt.Errorf("host /proc/self is %q", link)
	}
	want := map[int]bool{}
	for _, p := range inner {
		want[p] = true
	}
	tasks, err := os.ReadDir(filepath.Join(hostProc, link, "task"))
	if err != nil {
		return 0, nil, err
	}
	out := map[int]int{}
	for _, t := range tasks {
		children, err := os.ReadFile(filepath.Join(hostProc, link, "task", t.Name(), "children"))
		if err != nil {
			continue
		}
		for _, f := range strings.Fields(string(children)) {
			status, err := os.ReadFile(filepath.Join(hostProc, f, "status"))
			if err != nil {
				continue
			}
			nspid, err := parseNSpid(status)
			if err != nil {
				continue
			}
			host, err := strconv.Atoi(f)
			if err != nil {
				continue
			}
			if p := nspid[len(nspid)-1]; want[p] {
				out[p] = host
			}
		}
	}
	return self, out, nil
}

// reap waits for every child and orphan of the namespace, forever.
func reap() {
	for {
		var ws syscall.WaitStatus
		if _, err := syscall.Wait4(-1, &ws, 0, nil); errors.Is(err, syscall.ECHILD) {
			time.Sleep(time.Second)
		}
	}
}

func ipBatch(nsenter []string, lines []string, log io.Writer) error {
	name, args := "ip", []string{"-batch", "-"}
	if nsenter != nil {
		name, args = "nsenter", append(append(append([]string{}, nsenter...), "--", "ip"), args...)
	}
	cmd := exec.Command(name, args...)
	cmd.Stdin = strings.NewReader(strings.Join(lines, "\n") + "\n")
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ip -batch: %w", err)
	}
	return nil
}

func run(log io.Writer, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func writeDC(state string, dc *DC) error {
	raw, err := json.MarshalIndent(dc, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(state, dcFile+".tmp")
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(state, dcFile))
}

func mkdirCgroup(dir string) error {
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return nil
}

func writeFile(path, value string) error {
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		return fmt.Errorf("write %q to %s: %w", value, path, err)
	}
	return nil
}
