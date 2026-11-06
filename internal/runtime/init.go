package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Descriptors create hands to init; namespace files follow from 6 on.
const (
	initConfigFD = 3
	initSyncFD   = 4
	initFifoFD   = 5
)

const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// initConfig is what create sends init through the config pipe.
type initConfig struct {
	Spec      *Spec    `json:"spec"`
	Rootfs    string   `json:"rootfs"`
	Bundle    string   `json:"bundle"`
	Joins     []nsJoin `json:"joins,omitempty"`
	NewUserNS bool     `json:"newUserNS,omitempty"`
	CapLast   int      `json:"capLast"`
}

// initReply is init's answer on the sync pipe; an empty Error means the
// container is created.
type initReply struct {
	Error string `json:"error,omitempty"`
}

// runInit is the container init. It runs on the main thread, locked by
// the command before main, so every per-thread change (namespaces,
// credentials, capabilities) is the one exec keeps.
func runInit() {
	syncFile := os.NewFile(initSyncFD, "sync")
	c, err := prepareInit()
	reply := initReply{}
	if err != nil {
		reply.Error = err.Error()
	}
	if werr := json.NewEncoder(syncFile).Encode(reply); werr != nil || err != nil {
		os.Exit(1)
	}
	syncFile.Close()

	if err := waitStart(); err != nil {
		fmt.Fprintln(os.Stderr, "anden-runtime init:", err)
		os.Exit(1)
	}
	if c.Spec.Process == nil {
		os.Exit(0)
	}
	err = execProcess(c)
	fmt.Fprintln(os.Stderr, "anden-runtime init:", err)
	os.Exit(127)
}

func prepareInit() (*initConfig, error) {
	f := os.NewFile(initConfigFD, "config")
	data, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		return nil, fmt.Errorf("read init config: %w", err)
	}
	var c initConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("decode init config: %w", err)
	}
	if c.Spec == nil || c.Spec.Linux == nil || c.Spec.Root == nil {
		return nil, errors.New("init config without a spec")
	}
	syscall.CloseOnExec(initFifoFD)

	if err := joinNamespaces(c.Joins); err != nil {
		return nil, err
	}
	for _, j := range c.Joins {
		syscall.Close(j.FD)
	}
	spec := c.Spec
	if spec.Process != nil && spec.Process.OOMScoreAdj != nil {
		adj := strconv.Itoa(*spec.Process.OOMScoreAdj)
		if err := os.WriteFile("/proc/self/oom_score_adj", []byte(adj), 0); err != nil {
			return nil, fmt.Errorf("oomScoreAdj %s: %w", adj, err)
		}
	}
	if err := setupRootfs(&c); err != nil {
		return nil, err
	}
	if err := finalizeRootfs(spec); err != nil {
		return nil, err
	}
	if spec.Hostname != "" {
		if err := syscall.Sethostname([]byte(spec.Hostname)); err != nil {
			return nil, fmt.Errorf("sethostname: %w", err)
		}
	}
	if spec.Domainname != "" {
		if err := syscall.Setdomainname([]byte(spec.Domainname)); err != nil {
			return nil, fmt.Errorf("setdomainname: %w", err)
		}
	}
	// Own session, no controlling terminal and no tie to the caller: the
	// container survives the exit of create and of whoever ran it.
	if _, err := syscall.Setsid(); err != nil {
		return nil, fmt.Errorf("setsid: %w", err)
	}
	return &c, nil
}

// waitStart blocks until start writes a byte to the FIFO. init holds the
// FIFO open for writing too, so the read never sees end of file.
func waitStart() error {
	if err := syscall.SetNonblock(initFifoFD, false); err != nil {
		return err
	}
	buf := make([]byte, 1)
	for {
		n, err := syscall.Read(initFifoFD, buf)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("wait for start: %w", err)
		}
		if n == 1 {
			syscall.Close(initFifoFD)
			return nil
		}
	}
}

func execProcess(c *initConfig) error {
	p := c.Spec.Process
	if err := syscall.Chdir(p.Cwd); err != nil {
		return fmt.Errorf("chdir %s: %w", p.Cwd, err)
	}
	path, err := lookPath(p.Args[0], p.Env)
	if err != nil {
		return err
	}
	if err := applyRlimits(p.Rlimits); err != nil {
		return err
	}
	umask := 0o022
	if p.User.Umask != nil {
		umask = int(*p.User.Umask)
	}
	syscall.Umask(umask)
	// The kernel installs a filter only with no_new_privs or with
	// CAP_SYS_ADMIN. With noNewPrivileges false the filter goes in while
	// init still holds every capability, as runc does; otherwise right
	// before exec, after applyUser sets PR_SET_NO_NEW_PRIVS.
	if !p.NoNewPrivileges {
		if err := applySeccomp(c.Spec.Linux.Seccomp); err != nil {
			return err
		}
	}
	if err := applyUser(p, c.CapLast); err != nil {
		return err
	}
	if p.NoNewPrivileges {
		if err := applySeccomp(c.Spec.Linux.Seccomp); err != nil {
			return err
		}
	}
	if err := syscall.Exec(path, p.Args, p.Env); err != nil {
		return fmt.Errorf("exec %s: %w", path, err)
	}
	return nil
}

// lookPath finds args[0] in the PATH of process.env, inside the new root.
func lookPath(file string, env []string) (string, error) {
	if strings.Contains(file, "/") {
		return file, nil
	}
	path := defaultPath
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "PATH="); ok {
			path = v
		}
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, file)
		fi, err := os.Stat(candidate)
		if err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("executable %q not found in PATH %s", file, path)
}
