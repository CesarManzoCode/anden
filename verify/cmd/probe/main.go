// Command probe prints, as JSON on stdout, what a process can see and do from
// inside a container. It takes everything it compares against from the
// environment the acceptance script writes into the bundle:
//
//	PROBE_HOST_HOSTNAME  hostname of the host
//	PROBE_HOST_PID       PID of a live process on the host
//	PROBE_HOST_COMM      comm that process has on the host
//
// A missing value or an unmounted /proc exits with 2 instead of reporting an
// isolation it could not check.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// missingPath does not exist, so a syscall that gets past the filter fails
// with ENOENT or EPERM and never changes anything.
const missingPath = "/nonexistent-anden-probe"

// maxCommLen is what the kernel keeps of a process name: TASK_COMM_LEN minus the NUL.
const maxCommLen = 15

type report struct {
	PID1             bool              `json:"pid1"`
	HostnameOwn      bool              `json:"hostname_own"`
	HostProcsVisible bool              `json:"host_procs_visible"`
	BlockedSyscalls  map[string]string `json:"blocked_syscalls"`
}

func main() {
	rep, err := collect(os.Getenv, "/proc")
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		os.Exit(2)
	}
	if err := json.NewEncoder(os.Stdout).Encode(rep); err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		os.Exit(2)
	}
}

func collect(getenv func(string) string, procRoot string) (report, error) {
	hostHostname := getenv("PROBE_HOST_HOSTNAME")
	if hostHostname == "" {
		return report{}, errors.New("PROBE_HOST_HOSTNAME is not set")
	}
	own, err := nodename()
	if err != nil {
		return report{}, fmt.Errorf("uname: %w", err)
	}
	visible, err := hostProcVisible(procRoot, getenv("PROBE_HOST_PID"), getenv("PROBE_HOST_COMM"))
	if err != nil {
		return report{}, err
	}
	blocked := map[string]string{"mount": syscallOutcome(tryMount()), "swapon": syscallOutcome(trySwapon())}
	return report{PID1: os.Getpid() == 1, HostnameOwn: own != hostHostname, HostProcsVisible: visible, BlockedSyscalls: blocked}, nil
}

func nodename() (string, error) {
	var u syscall.Utsname
	if err := syscall.Uname(&u); err != nil {
		return "", err
	}
	name := make([]byte, 0, len(u.Nodename))
	for _, c := range u.Nodename {
		if c == 0 {
			break
		}
		name = append(name, byte(c))
	}
	return string(name), nil
}

// hostProcVisible reports whether procRoot shows the host process. A process
// with the same PID but another comm belongs to the container, not to the host.
func hostProcVisible(procRoot, pid, comm string) (bool, error) {
	if n, err := strconv.Atoi(pid); err != nil || n <= 0 {
		return false, fmt.Errorf("PROBE_HOST_PID is not a PID: %q", pid)
	}
	if comm == "" {
		return false, errors.New("PROBE_HOST_COMM is not set")
	}
	if _, err := os.Stat(filepath.Join(procRoot, "self")); err != nil {
		return false, fmt.Errorf("proc is not mounted at %s: %w", procRoot, err)
	}
	data, err := os.ReadFile(filepath.Join(procRoot, pid, "comm"))
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if len(comm) > maxCommLen {
		comm = comm[:maxCommLen]
	}
	return strings.TrimSuffix(string(data), "\n") == comm, nil
}

func tryMount() error {
	return syscall.Mount("none", missingPath, "tmpfs", 0, "")
}

func trySwapon() error {
	path, err := syscall.BytePtrFromString(missingPath)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall(syscall.SYS_SWAPON, uintptr(unsafe.Pointer(path)), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func syscallOutcome(err error) string {
	if err == nil {
		return "OK"
	}
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return err.Error()
	}
	switch errno {
	case syscall.EPERM:
		return "EPERM"
	case syscall.ENOENT:
		return "ENOENT"
	case syscall.EACCES:
		return "EACCES"
	case syscall.EINVAL:
		return "EINVAL"
	case syscall.ENOSYS:
		return "ENOSYS"
	}
	return "errno " + strconv.Itoa(int(errno))
}
