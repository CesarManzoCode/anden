package sim

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	andenv1 "github.com/CesarManzoCode/anden/gen/anden/v1"
)

// Up launches the datacenter detached, in the scope anden-<h>, and returns
// once dc.json exists and the three nodes are registered and ready. If that
// does not happen within timeout, it kills the scope and keeps the state
// for inspection: `anden down` removes it.
func Up(state string, timeout time.Duration, stderr io.Writer) (*DC, error) {
	abs, h, err := StatePath(state)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(abs, dcFile)); err == nil {
		return nil, fmt.Errorf("%s already has %s: run anden down first", abs, dcFile)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	logPath := filepath.Join(abs, logFile)
	log, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	defer log.Close()
	// A scope left failed by an earlier run would keep the unit name taken.
	_ = exec.Command("systemctl", "--user", "reset-failed", Unit(h)+".scope").Run()

	cmd := exec.Command("systemd-run", "--user", "--scope", "-q", "--unit="+Unit(h), "-p", "Delegate=yes",
		"--", self, "_dc", "--state", abs, "--sockets", SocketDir(h, os.Getenv("XDG_RUNTIME_DIR")))
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	fail := func(reason error) (*DC, error) {
		killScope(abs, h)
		if tail := tailFile(logPath, 40); tail != "" {
			fmt.Fprintf(stderr, "anden: last lines of %s:\n%s", logPath, tail)
		}
		return nil, fmt.Errorf("%w; the state stays in %s until anden down", reason, abs)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			return fail(fmt.Errorf("the datacenter exited during up: %v", err))
		case <-time.After(200 * time.Millisecond):
		}
		dc, err := ReadDC(abs)
		if err != nil {
			continue
		}
		if n, err := readyNodes(dc.APISocket); err == nil && n == NodeCount {
			return dc, nil
		}
	}
	return fail(fmt.Errorf("the datacenter was not ready in %s", timeout))
}

// readyNodes counts the registered nodes in state READY.
func readyNodes(socket string) (int, error) {
	conn, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := andenv1.NewAndenClient(conn).ListNodes(ctx, &andenv1.ListNodesRequest{})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, node := range resp.GetNodes() {
		if node.GetState() == andenv1.NodeState_NODE_STATE_READY {
			n++
		}
	}
	return n, nil
}

// Down kills the scope, waits until it is empty, removes its cgroups and
// deletes the state and the socket directory inside the id mapping,
// because the instances leave files owned by subordinate ids.
func Down(state string) error {
	abs, h, err := StatePath(state)
	if err != nil {
		return err
	}
	if err := killScope(abs, h); err != nil {
		return err
	}
	return removeState(abs, SocketDir(h, os.Getenv("XDG_RUNTIME_DIR")))
}

// killScope empties and removes the scope, found through dc.json or, when
// up failed before writing it, through systemd.
func killScope(abs, h string) error {
	scope := ""
	if dc, err := ReadDC(abs); err == nil {
		scope = dc.ScopeCgroup
	} else if out, err := exec.Command("systemctl", "--user", "show", "-P", "ControlGroup", Unit(h)+".scope").Output(); err == nil {
		if cg := strings.TrimSpace(string(out)); cg != "" {
			scope = filepath.Join("/sys/fs/cgroup", cg)
		}
	}
	defer exec.Command("systemctl", "--user", "reset-failed", Unit(h)+".scope").Run()
	if scope == "" {
		return nil
	}
	if _, err := os.Stat(scope); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := writeFile(filepath.Join(scope, "cgroup.kill"), "1"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := waitEmpty(scope, 10*time.Second); err != nil {
		return err
	}
	removeCgroupTree(scope)
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if _, err := os.Stat(scope); errors.Is(err, os.ErrNotExist) {
			return nil
		}
	}
	_ = exec.Command("systemctl", "--user", "stop", Unit(h)+".scope").Run()
	if err := os.Remove(scope); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("scope %s stays: %w", scope, err)
	}
	return nil
}

func waitEmpty(cgroup string, timeout time.Duration) error {
	for end := time.Now().Add(timeout); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		events, err := os.ReadFile(filepath.Join(cgroup, "cgroup.events"))
		if errors.Is(err, os.ErrNotExist) || bytes.Contains(events, []byte("populated 0")) {
			return nil
		}
	}
	return fmt.Errorf("%s still has processes after %s", cgroup, timeout)
}

// removeCgroupTree removes the cgroups below dir, deepest first. systemd
// removes the scope itself once it is empty.
func removeCgroupTree(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			sub := filepath.Join(dir, e.Name())
			removeCgroupTree(sub)
			_ = os.Remove(sub)
		}
	}
}

// removeState deletes the state directory with rm running as root of a
// user namespace with the subordinate ids. It refuses a directory that does
// not look like a datacenter state.
func removeState(abs, sockets string) error {
	if _, err := os.Stat(abs); errors.Is(err, os.ErrNotExist) {
		return os.RemoveAll(sockets)
	}
	home, _ := os.UserHomeDir()
	if abs == "/" || abs == home {
		return fmt.Errorf("refusing to delete %s", abs)
	}
	marked := false
	for _, f := range []string{dcFile, logFile} {
		if _, err := os.Stat(filepath.Join(abs, f)); err == nil {
			marked = true
		}
	}
	if !marked {
		return fmt.Errorf("%s has neither %s nor %s: not a datacenter state, not deleted", abs, dcFile, logFile)
	}
	out, err := exec.Command("unshare", "-U", "--map-root-user", "--map-auto", "--",
		"rm", "-rf", "--one-file-system", "--", abs, sockets).CombinedOutput()
	if err != nil {
		return fmt.Errorf("rm %s: %v: %s", abs, err, out)
	}
	return nil
}

// Exec runs cmd in the network namespace of ns (root, client or nodeN),
// inside the user namespace, and returns its exit code.
func Exec(state, ns string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	abs, _, err := StatePath(state)
	if err != nil {
		return 1, err
	}
	dc, err := ReadDC(abs)
	if err != nil {
		return 1, err
	}
	pid, ok := dc.NS[ns]
	if !ok {
		return 1, fmt.Errorf("unknown namespace %q; use root, client or node1..node%d", ns, NodeCount)
	}
	c := exec.Command("nsenter", ExecArgs(pid, cmd)...)
	c.Stdin, c.Stdout, c.Stderr = stdin, stdout, stderr
	if err := c.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode(), nil
		}
		return 1, err
	}
	return 0, nil
}

// ChaosKill crashes a node as D06 says: cgroup.kill of its cgroup, which
// takes the agent and every instance. Its netns and disk mount survive in
// hold/.
func ChaosKill(state, node string) error {
	if _, ok := FindNode(node); !ok {
		return fmt.Errorf("unknown node %q", node)
	}
	abs, _, err := StatePath(state)
	if err != nil {
		return err
	}
	dc, err := ReadDC(abs)
	if err != nil {
		return err
	}
	cg := filepath.Join(dc.ScopeCgroup, "dc", node)
	if err := writeFile(filepath.Join(cg, "cgroup.kill"), "1"); err != nil {
		return err
	}
	return waitEmpty(cg, 10*time.Second)
}

// ChaosNet injects a network fault of D06 (partition, heal or slow) on the
// link of a node, from the root netns of the datacenter.
func ChaosNet(state, action, node string, delayMs int) error {
	n, ok := FindNode(node)
	if !ok {
		return fmt.Errorf("unknown node %q", node)
	}
	steps, err := ChaosSteps(action, n, delayMs)
	if err != nil {
		return err
	}
	abs, _, err := StatePath(state)
	if err != nil {
		return err
	}
	dc, err := ReadDC(abs)
	if err != nil {
		return err
	}
	root, ok := dc.NS[RootName]
	if !ok {
		return fmt.Errorf("%s has no %s namespace", dcFile, RootName)
	}
	for _, step := range steps {
		out, err := exec.Command("nsenter", ExecArgs(root, step.Cmd)...).CombinedOutput()
		if err != nil && !step.Optional {
			return fmt.Errorf("%s: %w: %s", strings.Join(step.Cmd, " "), err, bytes.TrimSpace(out))
		}
	}
	return nil
}

func tailFile(path string, n int) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.SplitAfter(string(raw), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "")
}
