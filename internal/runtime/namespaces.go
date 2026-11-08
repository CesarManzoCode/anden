package runtime

import (
	"fmt"
	"os"
	"syscall"
)

// namespaceFlags maps linux.namespaces[].type to its clone flag.
var namespaceFlags = map[string]uintptr{
	"pid":     syscall.CLONE_NEWPID,
	"network": syscall.CLONE_NEWNET,
	"mount":   syscall.CLONE_NEWNS,
	"ipc":     syscall.CLONE_NEWIPC,
	"uts":     syscall.CLONE_NEWUTS,
	"user":    syscall.CLONE_NEWUSER,
	"cgroup":  syscall.CLONE_NEWCGROUP,
	"time":    syscall.CLONE_NEWTIME,
}

// nsGetNSType is NS_GET_NSTYPE, _IO(0xb7, 0x3) in linux/nsfs.h.
const nsGetNSType = 0xb703

// nsJoin is a namespace init enters with setns. FD is the descriptor
// number init receives it on.
type nsJoin struct {
	Type string `json:"type"`
	FD   int    `json:"fd"`
}

// namespacePlan splits linux.namespaces into what clone creates and what
// is joined by path. A user namespace without mappings is neither: the
// runtime already runs inside one and does not nest another.
type namespacePlan struct {
	CloneFlags uintptr
	NewUser    bool
	PidPath    string
	JoinPaths  []Namespace
}

func planNamespaces(l *Linux) namespacePlan {
	var plan namespacePlan
	hasMappings := len(l.UIDMappings) > 0
	for _, ns := range l.Namespaces {
		switch {
		case ns.Type == "user":
			if hasMappings {
				plan.CloneFlags |= syscall.CLONE_NEWUSER
				plan.NewUser = true
			}
		case ns.Path == "":
			plan.CloneFlags |= namespaceFlags[ns.Type]
		case ns.Type == "pid":
			// setns on a PID namespace only affects children, so the
			// parent joins it before forking init.
			plan.PidPath = ns.Path
		default:
			plan.JoinPaths = append(plan.JoinPaths, ns)
		}
	}
	return plan
}

// openNamespace opens a namespace file and checks that it is of the
// declared type, so a path of the wrong kind fails at create.
func openNamespace(ns Namespace) (*os.File, error) {
	f, err := os.Open(ns.Path)
	if err != nil {
		return nil, fmt.Errorf("namespace %s: %w", ns.Type, err)
	}
	got, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), nsGetNSType, 0)
	if e != 0 {
		f.Close()
		return nil, fmt.Errorf("namespace %s: %s is not a namespace file: %w", ns.Type, ns.Path, e)
	}
	if got != namespaceFlags[ns.Type] {
		f.Close()
		return nil, fmt.Errorf("namespace %s: %s is a namespace of another type", ns.Type, ns.Path)
	}
	return f, nil
}

func setns(fd int, kind string) error {
	if _, _, e := syscall.RawSyscall(sysSetns, uintptr(fd), namespaceFlags[kind], 0); e != 0 {
		return fmt.Errorf("setns %s: %w", kind, e)
	}
	return nil
}

// joinNamespaces enters the namespaces given by path on the calling
// thread, the one that will exec. A mount namespace cannot be joined while
// the thread shares its filesystem attributes with the other Go threads,
// so it first unshares CLONE_FS; the mount namespace goes last because the
// others were opened through paths of the current one.
func joinNamespaces(joins []nsJoin) error {
	var mount *nsJoin
	for i := range joins {
		if joins[i].Type == "mount" {
			mount = &joins[i]
			continue
		}
		if err := setns(joins[i].FD, joins[i].Type); err != nil {
			return err
		}
	}
	if mount == nil {
		return nil
	}
	if _, _, e := syscall.RawSyscall(syscall.SYS_UNSHARE, syscall.CLONE_FS, 0, 0); e != 0 {
		return fmt.Errorf("unshare CLONE_FS: %w", e)
	}
	return setns(mount.FD, "mount")
}
