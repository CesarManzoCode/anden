package agent

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"

	andenv1 "github.com/CesarManzoCode/anden/gen/anden/v1"
	"github.com/CesarManzoCode/anden/internal/image"
	ociruntime "github.com/CesarManzoCode/anden/internal/runtime"
)

const (
	defaultPath = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	cpuPeriod   = 100000
)

// capabilities are the ones an instance keeps. CAP_SYS_ADMIN is not among them.
var capabilities = []string{
	"CAP_AUDIT_WRITE", "CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FOWNER", "CAP_FSETID", "CAP_KILL", "CAP_NET_BIND_SERVICE",
	"CAP_SETFCAP", "CAP_SETGID", "CAP_SETPCAP", "CAP_SETUID", "CAP_SYS_CHROOT",
}

// blockedSyscalls answer EPERM inside an instance: a service has no business
// changing mounts or namespaces, loading kernel code or tracing others.
var blockedSyscalls = []string{
	"acct", "add_key", "adjtimex", "bpf", "clock_settime", "delete_module", "finit_module", "init_module", "kexec_file_load",
	"kexec_load", "keyctl", "mount", "open_by_handle_at", "perf_event_open", "pivot_root", "process_vm_readv",
	"process_vm_writev", "ptrace", "quotactl", "reboot", "request_key", "setns", "settimeofday", "swapoff", "swapon",
	"umount2", "unshare", "userfaultfd",
}

// configInput is everything config.json depends on.
type configInput struct {
	spec   *andenv1.ServiceSpec
	run    image.RunConfig
	rootfs string
	netNS  string
	utsNS  string
	ipcNS  string
	cgroup string
	// volumes is the directory of the node that holds one directory per volume.
	volumes string
}

// seccompEnabled says whether the instance runs with its filter: the restore
// route runs without it (D19).
func seccompEnabled(spec *andenv1.ServiceSpec) bool {
	return spec.GetWakeRoute() != andenv1.WakeRoute_WAKE_ROUTE_RESTORE
}

// processArgs is the entrypoint followed by the spec's args or, without them,
// the image's Cmd.
func processArgs(run image.RunConfig, args []string) ([]string, error) {
	if len(args) == 0 {
		args = run.Cmd
	}
	out := slices.Concat(run.Entrypoint, args)
	if len(out) == 0 {
		return nil, errors.New("the image has no Entrypoint or Cmd and the service no args")
	}
	return out, nil
}

// processEnv is the image's environment with the spec's on top, in a stable
// order, and a default PATH if neither has one.
func processEnv(run image.RunConfig, env map[string]string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, e := range run.Env {
		k, _, _ := strings.Cut(e, "=")
		if _, override := env[k]; !override {
			out = append(out, e)
			seen[k] = true
		}
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		out = append(out, k+"="+env[k])
		seen[k] = true
	}
	if !seen["PATH"] {
		out = append(out, defaultPath)
	}
	return out
}

// buildConfig generates the config.json of an instance: the namespaces the
// agent prepared, its own pid and mount namespaces, the limits of its spec and
// the seccomp filter.
func buildConfig(in configInput) (*ociruntime.Spec, error) {
	args, err := processArgs(in.run, in.spec.GetArgs())
	if err != nil {
		return nil, err
	}
	cwd := in.run.WorkingDir
	if cwd == "" {
		cwd = "/"
	}
	memory := int64(in.spec.GetMemoryMib()) << 20
	quota := int64(in.spec.GetCpuMillis()) * cpuPeriod / 1000
	period := uint64(cpuPeriod)
	resources := &ociruntime.Resources{
		Memory: &ociruntime.Memory{Limit: &memory},
		CPU:    &ociruntime.CPU{Quota: &quota, Period: &period},
	}
	if pids := int64(in.spec.GetPidsMax()); pids > 0 {
		resources.Pids = &ociruntime.Pids{Limit: &pids}
	}
	linux := &ociruntime.Linux{
		Resources:   resources,
		CgroupsPath: strings.TrimPrefix(in.cgroup, cgroupRoot),
		Namespaces: []ociruntime.Namespace{
			{Type: "pid"},
			{Type: "mount"},
			{Type: "network", Path: in.netNS},
			{Type: "uts", Path: in.utsNS},
			{Type: "ipc", Path: in.ipcNS},
		},
	}
	if seccompEnabled(in.spec) {
		linux.Seccomp = &ociruntime.Seccomp{
			DefaultAction: "SCMP_ACT_ALLOW",
			Architectures: []string{"SCMP_ARCH_X86_64"},
			Syscalls:      []ociruntime.SeccompSyscall{{Names: blockedSyscalls, Action: "SCMP_ACT_ERRNO"}},
		}
	}
	mounts := []ociruntime.Mount{
		{Destination: "/proc", Type: "proc", Source: "proc", Options: []string{"nosuid", "noexec", "nodev"}},
	}
	// The restore route keeps /dev in the rootfs, where the runtime binds the
	// device nodes: without root, criu writes the owner of a tmpfs as the host
	// sees it, a uid the user namespace does not map, so a tmpfs it restored
	// once cannot be restored again.
	if seccompEnabled(in.spec) {
		mounts = append(mounts, ociruntime.Mount{
			Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"},
		})
	}
	for _, v := range in.spec.GetVolumes() {
		mounts = append(mounts, ociruntime.Mount{
			Destination: v.GetMountPath(), Type: "bind", Source: filepath.Join(in.volumes, v.GetName()), Options: []string{"bind"},
		})
	}
	return &ociruntime.Spec{
		Version: ociruntime.OCIVersion,
		Process: &ociruntime.Process{
			Args: args,
			Env:  processEnv(in.run, in.spec.GetEnv()),
			Cwd:  cwd,
			Capabilities: &ociruntime.Capabilities{
				Bounding: capabilities, Effective: capabilities, Permitted: capabilities,
			},
			NoNewPrivileges: true,
		},
		Root:        &ociruntime.Root{Path: in.rootfs},
		Hostname:    in.spec.GetName(),
		Mounts:      mounts,
		Annotations: map[string]string{ociruntime.StdioAnnotation: "/.anden/stdio.log"},
		Linux:       linux,
	}, nil
}
