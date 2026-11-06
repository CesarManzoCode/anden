package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// OCIVersion is the runtime-spec version this runtime reports in its state.
const OCIVersion = "1.3.0"

// Spec is config.json as runtime-spec 1.3.0 defines it. Every field of the
// specification is declared so that strict decoding rejects typos; the ones
// outside the supported subset are kept raw and rejected by Validate.
type Spec struct {
	Version     string            `json:"ociVersion"`
	Process     *Process          `json:"process,omitempty"`
	Root        *Root             `json:"root,omitempty"`
	Hostname    string            `json:"hostname,omitempty"`
	Domainname  string            `json:"domainname,omitempty"`
	Mounts      []Mount           `json:"mounts,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Linux       *Linux            `json:"linux,omitempty"`

	Hooks   json.RawMessage `json:"hooks,omitempty"`
	Solaris json.RawMessage `json:"solaris,omitempty"`
	Windows json.RawMessage `json:"windows,omitempty"`
	VM      json.RawMessage `json:"vm,omitempty"`
	ZOS     json.RawMessage `json:"zos,omitempty"`
	FreeBSD json.RawMessage `json:"freebsd,omitempty"`
}

type Process struct {
	Terminal        bool            `json:"terminal,omitempty"`
	ConsoleSize     json.RawMessage `json:"consoleSize,omitempty"`
	User            User            `json:"user"`
	Args            []string        `json:"args,omitempty"`
	Env             []string        `json:"env,omitempty"`
	Cwd             string          `json:"cwd"`
	Capabilities    *Capabilities   `json:"capabilities,omitempty"`
	Rlimits         []Rlimit        `json:"rlimits,omitempty"`
	NoNewPrivileges bool            `json:"noNewPrivileges,omitempty"`
	OOMScoreAdj     *int            `json:"oomScoreAdj,omitempty"`
	CommandLine     string          `json:"commandLine,omitempty"`
	ApparmorProfile string          `json:"apparmorProfile,omitempty"`
	SelinuxLabel    string          `json:"selinuxLabel,omitempty"`
	Scheduler       json.RawMessage `json:"scheduler,omitempty"`
	IOPriority      json.RawMessage `json:"ioPriority,omitempty"`
	ExecCPUAffinity json.RawMessage `json:"execCPUAffinity,omitempty"`
}

type User struct {
	UID            uint32   `json:"uid"`
	GID            uint32   `json:"gid"`
	Umask          *uint32  `json:"umask,omitempty"`
	AdditionalGids []uint32 `json:"additionalGids,omitempty"`
	Username       string   `json:"username,omitempty"`
}

type Capabilities struct {
	Bounding    []string `json:"bounding,omitempty"`
	Effective   []string `json:"effective,omitempty"`
	Inheritable []string `json:"inheritable,omitempty"`
	Permitted   []string `json:"permitted,omitempty"`
	Ambient     []string `json:"ambient,omitempty"`
}

type Rlimit struct {
	Type string `json:"type"`
	Hard uint64 `json:"hard"`
	Soft uint64 `json:"soft"`
}

type Root struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly,omitempty"`
}

type Mount struct {
	Destination string          `json:"destination"`
	Type        string          `json:"type,omitempty"`
	Source      string          `json:"source,omitempty"`
	Options     []string        `json:"options,omitempty"`
	UIDMappings json.RawMessage `json:"uidMappings,omitempty"`
	GIDMappings json.RawMessage `json:"gidMappings,omitempty"`
}

type Linux struct {
	UIDMappings       []IDMapping       `json:"uidMappings,omitempty"`
	GIDMappings       []IDMapping       `json:"gidMappings,omitempty"`
	Sysctl            map[string]string `json:"sysctl,omitempty"`
	Resources         *Resources        `json:"resources,omitempty"`
	CgroupsPath       string            `json:"cgroupsPath,omitempty"`
	Namespaces        []Namespace       `json:"namespaces,omitempty"`
	Devices           []Device          `json:"devices,omitempty"`
	Seccomp           *Seccomp          `json:"seccomp,omitempty"`
	RootfsPropagation string            `json:"rootfsPropagation,omitempty"`
	MaskedPaths       []string          `json:"maskedPaths,omitempty"`
	ReadonlyPaths     []string          `json:"readonlyPaths,omitempty"`

	NetDevices   json.RawMessage `json:"netDevices,omitempty"`
	MountLabel   string          `json:"mountLabel,omitempty"`
	IntelRdt     json.RawMessage `json:"intelRdt,omitempty"`
	MemoryPolicy json.RawMessage `json:"memoryPolicy,omitempty"`
	Personality  json.RawMessage `json:"personality,omitempty"`
	TimeOffsets  json.RawMessage `json:"timeOffsets,omitempty"`
}

type IDMapping struct {
	ContainerID uint32 `json:"containerID"`
	HostID      uint32 `json:"hostID"`
	Size        uint32 `json:"size"`
}

type Namespace struct {
	Type string `json:"type"`
	Path string `json:"path,omitempty"`
}

type Device struct {
	Path     string  `json:"path"`
	Type     string  `json:"type"`
	Major    int64   `json:"major"`
	Minor    int64   `json:"minor"`
	FileMode *uint32 `json:"fileMode,omitempty"`
	UID      *uint32 `json:"uid,omitempty"`
	GID      *uint32 `json:"gid,omitempty"`
}

type Resources struct {
	Devices []DeviceRule `json:"devices,omitempty"`
	Memory  *Memory      `json:"memory,omitempty"`
	CPU     *CPU         `json:"cpu,omitempty"`
	Pids    *Pids        `json:"pids,omitempty"`

	BlockIO        json.RawMessage `json:"blockIO,omitempty"`
	HugepageLimits json.RawMessage `json:"hugepageLimits,omitempty"`
	Network        json.RawMessage `json:"network,omitempty"`
	Rdma           json.RawMessage `json:"rdma,omitempty"`
	Unified        json.RawMessage `json:"unified,omitempty"`
}

type DeviceRule struct {
	Allow  bool   `json:"allow"`
	Type   string `json:"type,omitempty"`
	Major  *int64 `json:"major,omitempty"`
	Minor  *int64 `json:"minor,omitempty"`
	Access string `json:"access,omitempty"`
}

type Memory struct {
	Limit *int64 `json:"limit,omitempty"`

	Reservation       json.RawMessage `json:"reservation,omitempty"`
	Swap              json.RawMessage `json:"swap,omitempty"`
	Kernel            json.RawMessage `json:"kernel,omitempty"`
	KernelTCP         json.RawMessage `json:"kernelTCP,omitempty"`
	Swappiness        json.RawMessage `json:"swappiness,omitempty"`
	DisableOOMKiller  json.RawMessage `json:"disableOOMKiller,omitempty"`
	UseHierarchy      json.RawMessage `json:"useHierarchy,omitempty"`
	CheckBeforeUpdate json.RawMessage `json:"checkBeforeUpdate,omitempty"`
}

type CPU struct {
	Quota  *int64  `json:"quota,omitempty"`
	Period *uint64 `json:"period,omitempty"`

	Shares          json.RawMessage `json:"shares,omitempty"`
	Burst           json.RawMessage `json:"burst,omitempty"`
	RealtimeRuntime json.RawMessage `json:"realtimeRuntime,omitempty"`
	RealtimePeriod  json.RawMessage `json:"realtimePeriod,omitempty"`
	Cpus            string          `json:"cpus,omitempty"`
	Mems            string          `json:"mems,omitempty"`
	Idle            json.RawMessage `json:"idle,omitempty"`
}

type Pids struct {
	Limit *int64 `json:"limit,omitempty"`
}

// Seccomp is linux.seccomp. The filter itself is compiled in seccomp.go.
type Seccomp struct {
	DefaultAction    string           `json:"defaultAction"`
	DefaultErrnoRet  *uint            `json:"defaultErrnoRet,omitempty"`
	Architectures    []string         `json:"architectures,omitempty"`
	Flags            []string         `json:"flags,omitempty"`
	ListenerPath     string           `json:"listenerPath,omitempty"`
	ListenerMetadata string           `json:"listenerMetadata,omitempty"`
	Syscalls         []SeccompSyscall `json:"syscalls,omitempty"`
}

type SeccompSyscall struct {
	Names    []string     `json:"names"`
	Action   string       `json:"action"`
	ErrnoRet *uint        `json:"errnoRet,omitempty"`
	Args     []SeccompArg `json:"args,omitempty"`
}

type SeccompArg struct {
	Index    uint   `json:"index"`
	Value    uint64 `json:"value"`
	ValueTwo uint64 `json:"valueTwo,omitempty"`
	Op       string `json:"op"`
}

// LoadSpec reads and validates config.json from a bundle directory.
func LoadSpec(bundle string) (*Spec, error) {
	data, err := os.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		return nil, err
	}
	spec, err := ParseSpec(data)
	if err != nil {
		return nil, fmt.Errorf("config.json: %w", err)
	}
	return spec, nil
}

// ParseSpec decodes config.json strictly and validates it against the
// supported subset.
func ParseSpec(data []byte) (*Spec, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var spec Spec
	if err := dec.Decode(&spec); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after the JSON object")
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return &spec, nil
}

// present reports whether a raw field carries a value. null, "", [] and {}
// count as absent.
func present(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	switch s {
	case "", "null", `""`, "[]", "{}":
		return false
	}
	return true
}

func unsupported(field string) error {
	return fmt.Errorf("%s is not supported by anden-runtime", field)
}

// Validate checks the subset of D09. Anything outside it is an error, never
// a silently ignored field.
func (s *Spec) Validate() error {
	if !strings.HasPrefix(s.Version, "1.") {
		return fmt.Errorf("ociVersion %q is not a 1.x runtime-spec version", s.Version)
	}
	for field, raw := range map[string]json.RawMessage{
		"hooks": s.Hooks, "solaris": s.Solaris, "windows": s.Windows,
		"vm": s.VM, "zos": s.ZOS, "freebsd": s.FreeBSD,
	} {
		if present(raw) {
			return unsupported(field)
		}
	}
	if s.Root == nil || s.Root.Path == "" {
		return errors.New("root.path is required")
	}
	if s.Linux == nil {
		return errors.New("linux is required")
	}
	if s.Process != nil {
		if err := s.Process.validate(); err != nil {
			return err
		}
	}
	for i, m := range s.Mounts {
		if err := m.validate(); err != nil {
			return fmt.Errorf("mounts[%d]: %w", i, err)
		}
	}
	if err := s.Linux.validate(); err != nil {
		return err
	}
	if s.Hostname != "" || s.Domainname != "" {
		if _, ok := s.Linux.namespace("uts"); !ok {
			return errors.New("hostname and domainname need a uts namespace")
		}
	}
	return nil
}

func (p *Process) validate() error {
	if p.Terminal {
		return unsupported("process.terminal")
	}
	for field, raw := range map[string]json.RawMessage{
		"process.scheduler": p.Scheduler, "process.ioPriority": p.IOPriority,
		"process.execCPUAffinity": p.ExecCPUAffinity, "process.consoleSize": p.ConsoleSize,
	} {
		if present(raw) {
			return unsupported(field)
		}
	}
	switch {
	case p.CommandLine != "":
		return unsupported("process.commandLine")
	case p.ApparmorProfile != "":
		return unsupported("process.apparmorProfile")
	case p.SelinuxLabel != "":
		return unsupported("process.selinuxLabel")
	case p.User.Username != "":
		return unsupported("process.user.username")
	}
	if len(p.Args) == 0 {
		return errors.New("process.args must not be empty")
	}
	if !filepath.IsAbs(p.Cwd) {
		return fmt.Errorf("process.cwd %q must be an absolute path", p.Cwd)
	}
	for _, e := range p.Env {
		if !strings.Contains(e, "=") {
			return fmt.Errorf("process.env entry %q has no '='", e)
		}
	}
	if p.Capabilities != nil {
		c := p.Capabilities
		for _, set := range [][]string{c.Bounding, c.Effective, c.Inheritable, c.Permitted, c.Ambient} {
			for _, name := range set {
				if _, ok := capabilityNumber(name); !ok {
					return fmt.Errorf("process.capabilities: unknown capability %q", name)
				}
			}
		}
	}
	for _, r := range p.Rlimits {
		if _, ok := rlimitNumber(r.Type); !ok {
			return fmt.Errorf("process.rlimits: unknown type %q", r.Type)
		}
		if r.Soft > r.Hard {
			return fmt.Errorf("process.rlimits: %s soft limit is above the hard limit", r.Type)
		}
	}
	if p.OOMScoreAdj != nil && (*p.OOMScoreAdj < -1000 || *p.OOMScoreAdj > 1000) {
		return fmt.Errorf("process.oomScoreAdj %d is outside [-1000, 1000]", *p.OOMScoreAdj)
	}
	return nil
}

func (m Mount) validate() error {
	if !filepath.IsAbs(m.Destination) {
		return fmt.Errorf("destination %q must be an absolute path", m.Destination)
	}
	if present(m.UIDMappings) || present(m.GIDMappings) {
		return unsupported("idmapped mounts")
	}
	if _, err := parseMountOptions(m.Options); err != nil {
		return err
	}
	return nil
}

var propagationModes = map[string]bool{
	"shared": true, "rshared": true, "slave": true, "rslave": true,
	"private": true, "rprivate": true, "unbindable": true, "runbindable": true,
}

func (l *Linux) namespace(kind string) (Namespace, bool) {
	for _, ns := range l.Namespaces {
		if ns.Type == kind {
			return ns, true
		}
	}
	return Namespace{}, false
}

func (l *Linux) validate() error {
	for field, raw := range map[string]json.RawMessage{
		"linux.netDevices": l.NetDevices, "linux.intelRdt": l.IntelRdt,
		"linux.memoryPolicy": l.MemoryPolicy, "linux.personality": l.Personality,
		"linux.timeOffsets": l.TimeOffsets,
	} {
		if present(raw) {
			return unsupported(field)
		}
	}
	if l.MountLabel != "" {
		return unsupported("linux.mountLabel")
	}
	seen := map[string]bool{}
	for _, ns := range l.Namespaces {
		if _, ok := namespaceFlags[ns.Type]; !ok {
			return fmt.Errorf("linux.namespaces: unknown type %q", ns.Type)
		}
		if seen[ns.Type] {
			return fmt.Errorf("linux.namespaces: type %q appears twice", ns.Type)
		}
		seen[ns.Type] = true
		if ns.Path != "" && !filepath.IsAbs(ns.Path) {
			return fmt.Errorf("linux.namespaces: path %q must be absolute", ns.Path)
		}
		switch {
		case ns.Type == "time":
			return unsupported("the time namespace")
		case ns.Type == "user" && ns.Path != "":
			return unsupported("joining a user namespace by path")
		}
	}
	if !seen["mount"] {
		return errors.New("linux.namespaces must include a mount namespace")
	}
	hasMappings := len(l.UIDMappings) > 0 || len(l.GIDMappings) > 0
	if hasMappings && !seen["user"] {
		return errors.New("linux.uidMappings and gidMappings need a user namespace")
	}
	if seen["user"] && hasMappings && (len(l.UIDMappings) == 0 || len(l.GIDMappings) == 0) {
		return errors.New("a new user namespace needs both uidMappings and gidMappings")
	}
	for _, d := range l.Devices {
		if !filepath.IsAbs(d.Path) {
			return fmt.Errorf("linux.devices: path %q must be absolute", d.Path)
		}
		if _, ok := deviceModes[d.Type]; !ok {
			return fmt.Errorf("linux.devices: %s has unknown type %q", d.Path, d.Type)
		}
	}
	for key := range l.Sysctl {
		if key == "" || strings.Contains(key, "..") {
			return fmt.Errorf("linux.sysctl: invalid key %q", key)
		}
	}
	if l.RootfsPropagation != "" && !propagationModes[l.RootfsPropagation] {
		return fmt.Errorf("linux.rootfsPropagation %q is not a propagation mode", l.RootfsPropagation)
	}
	for _, p := range append(append([]string{}, l.MaskedPaths...), l.ReadonlyPaths...) {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("masked and readonly paths must be absolute, got %q", p)
		}
	}
	if l.Resources != nil {
		if err := l.Resources.validate(); err != nil {
			return err
		}
	}
	return validateSeccomp(l.Seccomp)
}

func (r *Resources) validate() error {
	for field, raw := range map[string]json.RawMessage{
		"linux.resources.blockIO": r.BlockIO, "linux.resources.hugepageLimits": r.HugepageLimits,
		"linux.resources.network": r.Network, "linux.resources.rdma": r.Rdma,
		"linux.resources.unified": r.Unified,
	} {
		if present(raw) {
			return unsupported(field)
		}
	}
	// Device access is not enforced through cgroups (that needs eBPF); the
	// container only reaches the nodes the runtime binds. A deny-all rule
	// says exactly that, so it is the only rule accepted.
	for _, d := range r.Devices {
		if d.Allow || (d.Type != "" && d.Type != "a") || d.Major != nil || d.Minor != nil {
			return unsupported("linux.resources.devices rules other than deny-all")
		}
	}
	if m := r.Memory; m != nil {
		for field, raw := range map[string]json.RawMessage{
			"reservation": m.Reservation, "swap": m.Swap, "kernel": m.Kernel,
			"kernelTCP": m.KernelTCP, "swappiness": m.Swappiness,
			"disableOOMKiller": m.DisableOOMKiller, "useHierarchy": m.UseHierarchy,
			"checkBeforeUpdate": m.CheckBeforeUpdate,
		} {
			if present(raw) {
				return unsupported("linux.resources.memory." + field)
			}
		}
	}
	if c := r.CPU; c != nil {
		for field, raw := range map[string]json.RawMessage{
			"shares": c.Shares, "burst": c.Burst, "realtimeRuntime": c.RealtimeRuntime,
			"realtimePeriod": c.RealtimePeriod, "idle": c.Idle,
		} {
			if present(raw) {
				return unsupported("linux.resources.cpu." + field)
			}
		}
		if c.Cpus != "" || c.Mems != "" {
			return unsupported("linux.resources.cpu.cpus and mems")
		}
		if c.Period != nil && *c.Period == 0 {
			return errors.New("linux.resources.cpu.period must be positive")
		}
	}
	return nil
}
