package runtime

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// capabilityNames is the x86-64 numbering from linux/capability.h.
var capabilityNames = []string{
	"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_DAC_READ_SEARCH", "CAP_FOWNER",
	"CAP_FSETID", "CAP_KILL", "CAP_SETGID", "CAP_SETUID", "CAP_SETPCAP",
	"CAP_LINUX_IMMUTABLE", "CAP_NET_BIND_SERVICE", "CAP_NET_BROADCAST",
	"CAP_NET_ADMIN", "CAP_NET_RAW", "CAP_IPC_LOCK", "CAP_IPC_OWNER",
	"CAP_SYS_MODULE", "CAP_SYS_RAWIO", "CAP_SYS_CHROOT", "CAP_SYS_PTRACE",
	"CAP_SYS_PACCT", "CAP_SYS_ADMIN", "CAP_SYS_BOOT", "CAP_SYS_NICE",
	"CAP_SYS_RESOURCE", "CAP_SYS_TIME", "CAP_SYS_TTY_CONFIG", "CAP_MKNOD",
	"CAP_LEASE", "CAP_AUDIT_WRITE", "CAP_AUDIT_CONTROL", "CAP_SETFCAP",
	"CAP_MAC_OVERRIDE", "CAP_MAC_ADMIN", "CAP_SYSLOG", "CAP_WAKE_ALARM",
	"CAP_BLOCK_SUSPEND", "CAP_AUDIT_READ", "CAP_PERFMON", "CAP_BPF",
	"CAP_CHECKPOINT_RESTORE",
}

func capabilityNumber(name string) (int, bool) {
	for i, n := range capabilityNames {
		if n == name {
			return i, true
		}
	}
	return 0, false
}

var rlimitNames = map[string]int{
	"RLIMIT_CPU": 0, "RLIMIT_FSIZE": 1, "RLIMIT_DATA": 2, "RLIMIT_STACK": 3,
	"RLIMIT_CORE": 4, "RLIMIT_RSS": 5, "RLIMIT_NPROC": 6, "RLIMIT_NOFILE": 7,
	"RLIMIT_MEMLOCK": 8, "RLIMIT_AS": 9, "RLIMIT_LOCKS": 10, "RLIMIT_SIGPENDING": 11,
	"RLIMIT_MSGQUEUE": 12, "RLIMIT_NICE": 13, "RLIMIT_RTPRIO": 14, "RLIMIT_RTTIME": 15,
}

func rlimitNumber(name string) (int, bool) {
	n, ok := rlimitNames[name]
	return n, ok
}

const (
	linuxCapabilityVersion3 = 0x20080522
	prSetNoNewPrivs         = 38
	prCapAmbient            = 47
	prCapAmbientRaise       = 2
	prCapAmbientClearAll    = 4
)

// lastCapability reads the highest capability the kernel knows. A
// capability above it cannot be granted and is skipped.
func lastCapability() int {
	data, err := os.ReadFile("/proc/sys/kernel/cap_last_cap")
	if err != nil {
		return len(capabilityNames) - 1
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || n < 0 {
		return len(capabilityNames) - 1
	}
	// capset carries two 32-bit words.
	return min(n, 63)
}

// capMask turns capability names into the two 32-bit words capset uses.
func capMask(names []string, last int) [2]uint32 {
	var m [2]uint32
	for _, name := range names {
		n, ok := capabilityNumber(name)
		if !ok || n > last {
			continue
		}
		m[n/32] |= 1 << (uint(n) % 32)
	}
	return m
}

func hasCap(m [2]uint32, n int) bool {
	return m[n/32]&(1<<(uint(n)%32)) != 0
}

type capHeader struct {
	version uint32
	pid     int32
}

type capData struct {
	effective   uint32
	permitted   uint32
	inheritable uint32
}

// applyUser switches the calling thread to process.user with
// process.capabilities. It must run on the thread that calls exec.
func applyUser(p *Process, last int) error {
	caps := p.Capabilities
	if caps == nil {
		caps = &Capabilities{}
	}
	bounding := capMask(caps.Bounding, last)
	for n := 0; n <= last; n++ {
		if hasCap(bounding, n) {
			continue
		}
		if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_CAPBSET_DROP, uintptr(n), 0); e != 0 {
			return fmt.Errorf("drop %s from the bounding set: %w", capName(n), e)
		}
	}

	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_KEEPCAPS, 1, 0); e != 0 {
		return fmt.Errorf("PR_SET_KEEPCAPS: %w", e)
	}
	if err := setIDs(p.User); err != nil {
		return err
	}
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_KEEPCAPS, 0, 0); e != 0 {
		return fmt.Errorf("PR_SET_KEEPCAPS: %w", e)
	}

	eff, perm, inh := capMask(caps.Effective, last), capMask(caps.Permitted, last), capMask(caps.Inheritable, last)
	hdr := capHeader{version: linuxCapabilityVersion3}
	var data [2]capData
	for i := range data {
		data[i] = capData{effective: eff[i], permitted: perm[i], inheritable: inh[i]}
	}
	if _, _, e := syscall.RawSyscall(syscall.SYS_CAPSET, uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0); e != 0 {
		return fmt.Errorf("capset: %w", e)
	}

	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, prCapAmbient, prCapAmbientClearAll, 0, 0, 0, 0); e != 0 {
		return fmt.Errorf("clear ambient capabilities: %w", e)
	}
	ambient := capMask(caps.Ambient, last)
	for n := 0; n <= last; n++ {
		if !hasCap(ambient, n) {
			continue
		}
		if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, prCapAmbient, prCapAmbientRaise, uintptr(n), 0, 0, 0); e != 0 {
			return fmt.Errorf("raise ambient %s (it must also be permitted and inheritable): %w", capName(n), e)
		}
	}

	if p.NoNewPrivileges {
		if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); e != 0 {
			return fmt.Errorf("PR_SET_NO_NEW_PRIVS: %w", e)
		}
	}
	return nil
}

func capName(n int) string {
	if n < len(capabilityNames) {
		return capabilityNames[n]
	}
	return "capability " + strconv.Itoa(n)
}

// setIDs changes groups, gid and uid of the calling thread only. The
// process execs right after, so the other Go threads do not matter.
func setIDs(u User) error {
	groups := make([]uint32, len(u.AdditionalGids))
	copy(groups, u.AdditionalGids)
	var gp unsafe.Pointer
	if len(groups) > 0 {
		gp = unsafe.Pointer(&groups[0])
	}
	if _, _, e := syscall.RawSyscall(syscall.SYS_SETGROUPS, uintptr(len(groups)), uintptr(gp), 0); e != 0 {
		// A user namespace with setgroups denied still allows running
		// without supplementary groups.
		if !(e == syscall.EPERM && len(groups) == 0 && setgroupsDenied()) {
			return fmt.Errorf("setgroups %v: %w", groups, e)
		}
	}
	if _, _, e := syscall.RawSyscall(syscall.SYS_SETRESGID, uintptr(u.GID), uintptr(u.GID), uintptr(u.GID)); e != 0 {
		return fmt.Errorf("setresgid %d: %w", u.GID, e)
	}
	if _, _, e := syscall.RawSyscall(syscall.SYS_SETRESUID, uintptr(u.UID), uintptr(u.UID), uintptr(u.UID)); e != 0 {
		return fmt.Errorf("setresuid %d: %w", u.UID, e)
	}
	return nil
}

func setgroupsDenied() bool {
	data, err := os.ReadFile("/proc/self/setgroups")
	return err == nil && strings.TrimSpace(string(data)) == "deny"
}

// applyRlimits uses syscall.Setrlimit on purpose: for RLIMIT_NOFILE it
// stops syscall.Exec from restoring the soft limit Go saved at startup.
func applyRlimits(limits []Rlimit) error {
	for _, r := range limits {
		n, _ := rlimitNumber(r.Type)
		if err := syscall.Setrlimit(n, &syscall.Rlimit{Cur: r.Soft, Max: r.Hard}); err != nil {
			return fmt.Errorf("setrlimit %s: %w", r.Type, err)
		}
	}
	return nil
}
