package runtime

import (
	"strings"
	"testing"
)

// defaultConfig is the shape runtime-tools' generator writes, without the
// seccomp profile.
const defaultConfig = `{
	"ociVersion": "1.3.0",
	"process": {
		"user": {"uid": 0, "gid": 0},
		"args": ["/runtimetest", "--path=/"],
		"env": ["PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "TERM=xterm"],
		"cwd": "/",
		"capabilities": {
			"bounding": ["CAP_CHOWN", "CAP_KILL"],
			"effective": ["CAP_CHOWN", "CAP_KILL"],
			"inheritable": ["CAP_CHOWN", "CAP_KILL"],
			"permitted": ["CAP_CHOWN", "CAP_KILL"],
			"ambient": ["CAP_CHOWN", "CAP_KILL"]
		},
		"rlimits": [{"type": "RLIMIT_NOFILE", "hard": 1024, "soft": 1024}],
		"oomScoreAdj": 100
	},
	"root": {"path": "rootfs"},
	"hostname": "mrsdalloway",
	"mounts": [
		{"destination": "/proc", "type": "proc", "source": "proc", "options": ["nosuid", "noexec", "nodev"]},
		{"destination": "/dev", "type": "tmpfs", "source": "tmpfs", "options": ["nosuid", "strictatime", "mode=755", "size=65536k"]},
		{"destination": "/sys", "type": "sysfs", "source": "sysfs", "options": ["nosuid", "noexec", "nodev", "ro"]}
	],
	"annotations": {"TestName": "x"},
	"linux": {
		"resources": {"devices": [{"allow": false, "access": "rwm"}]},
		"namespaces": [{"type": "pid"}, {"type": "network"}, {"type": "ipc"}, {"type": "uts"}, {"type": "mount"}],
		"sysctl": {"net.ipv4.ip_forward": "1"},
		"maskedPaths": ["/proc/kcore"],
		"readonlyPaths": ["/proc/fs"],
		"rootfsPropagation": "shared"
	}
}`

func TestParseDefaultConfig(t *testing.T) {
	spec, err := ParseSpec([]byte(defaultConfig))
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	if spec.Process.Args[0] != "/runtimetest" || spec.Root.Path != "rootfs" || spec.Hostname != "mrsdalloway" {
		t.Fatalf("unexpected spec: %+v", spec)
	}
	if *spec.Process.OOMScoreAdj != 100 || spec.Process.Rlimits[0].Soft != 1024 {
		t.Fatalf("process fields lost: %+v", spec.Process)
	}
	if len(spec.Mounts) != 3 || spec.Mounts[1].Options[3] != "size=65536k" {
		t.Fatalf("mounts lost: %+v", spec.Mounts)
	}
	if spec.Linux.Sysctl["net.ipv4.ip_forward"] != "1" || spec.Linux.RootfsPropagation != "shared" {
		t.Fatalf("linux fields lost: %+v", spec.Linux)
	}
	if spec.Annotations["TestName"] != "x" {
		t.Fatalf("annotations lost: %+v", spec.Annotations)
	}
}

func TestParseResources(t *testing.T) {
	cfg := strings.Replace(defaultConfig, `"resources": {"devices": [{"allow": false, "access": "rwm"}]}`,
		`"cgroupsPath": "/anden/c1", "resources": {"memory": {"limit": 67108864}, "cpu": {"quota": 50000, "period": 100000}, "pids": {"limit": 64}}`, 1)
	spec, err := ParseSpec([]byte(cfg))
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	r := spec.Linux.Resources
	if *r.Memory.Limit != 67108864 || *r.CPU.Quota != 50000 || *r.CPU.Period != 100000 || *r.Pids.Limit != 64 {
		t.Fatalf("resources lost: %+v", r)
	}
	if spec.Linux.CgroupsPath != "/anden/c1" {
		t.Fatalf("cgroupsPath lost: %q", spec.Linux.CgroupsPath)
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		name, old, new, want string
	}{
		{"unknown field", `"hostname"`, `"hostnam"`, "unknown field"},
		{"old version", `"ociVersion": "1.3.0"`, `"ociVersion": "0.9.0"`, "ociVersion"},
		{"hooks", `"hostname"`, `"hooks": {"prestart": [{"path": "/bin/true"}]}, "hostname"`, "hooks"},
		{"terminal", `"user": {`, `"terminal": true, "user": {`, "process.terminal"},
		{"empty args", `"args": ["/runtimetest", "--path=/"]`, `"args": []`, "process.args"},
		{"relative cwd", `"cwd": "/"`, `"cwd": "tmp"`, "process.cwd"},
		{"unknown capability", `"bounding": ["CAP_CHOWN"`, `"bounding": ["CAP_FLY"`, "CAP_FLY"},
		{"unknown rlimit", `"RLIMIT_NOFILE"`, `"RLIMIT_SPOONS"`, "RLIMIT_SPOONS"},
		{"soft above hard", `"hard": 1024, "soft": 1024`, `"hard": 10, "soft": 1024`, "soft limit"},
		{"oom range", `"oomScoreAdj": 100`, `"oomScoreAdj": 1001`, "oomScoreAdj"},
		{"relative mount", `"destination": "/proc"`, `"destination": "proc"`, "absolute"},
		{"idmap option", `"options": ["nosuid", "noexec", "nodev"]}`, `"options": ["idmap"]}`, "idmap"},
		{"unknown namespace", `{"type": "pid"}`, `{"type": "pig"}`, "unknown type"},
		{"duplicate namespace", `{"type": "ipc"}`, `{"type": "pid"}`, "twice"},
		{"no mount namespace", `, {"type": "mount"}`, ``, "mount namespace"},
		{"time namespace", `{"type": "ipc"}`, `{"type": "time"}`, "time namespace"},
		{"user namespace path", `{"type": "ipc"}`, `{"type": "user", "path": "/proc/1/ns/user"}`, "user namespace by path"},
		{"mappings without user namespace", `"sysctl"`, `"uidMappings": [{"containerID": 0, "hostID": 1000, "size": 1}], "sysctl"`, "need a user namespace"},
		{"hostname without uts", `{"type": "uts"}, `, ``, "uts namespace"},
		{"bad propagation", `"rootfsPropagation": "shared"`, `"rootfsPropagation": "sideways"`, "propagation"},
		{"device allow rule", `{"allow": false, "access": "rwm"}`, `{"allow": true, "type": "c", "major": 1, "minor": 3, "access": "rwm"}`, "deny-all"},
		{"memory swap", `"devices": [{"allow": false, "access": "rwm"}]`, `"memory": {"swap": 1}`, "memory.swap"},
		{"cpu shares", `"devices": [{"allow": false, "access": "rwm"}]`, `"cpu": {"shares": 1024}`, "cpu.shares"},
		{"cpuset", `"devices": [{"allow": false, "access": "rwm"}]`, `"cpu": {"cpus": "0-1"}`, "cpus"},
		{"blockIO", `"devices": [{"allow": false, "access": "rwm"}]`, `"blockIO": {"weight": 10}`, "blockIO"},
		{"apparmor", `"cwd": "/"`, `"cwd": "/", "apparmorProfile": "x"`, "apparmor"},
		{"personality", `"sysctl"`, `"personality": {"domain": "LINUX"}, "sysctl"`, "personality"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := strings.Replace(defaultConfig, c.old, c.new, 1)
			if cfg == defaultConfig {
				t.Fatalf("replacement %q not found", c.old)
			}
			_, err := ParseSpec([]byte(cfg))
			if err == nil {
				t.Fatal("ParseSpec accepted the config")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

func TestParseNullAndEmptyUnsupportedFields(t *testing.T) {
	cfg := strings.Replace(defaultConfig, `"hostname"`, `"hooks": null, "windows": {}, "hostname"`, 1)
	if _, err := ParseSpec([]byte(cfg)); err != nil {
		t.Fatalf("empty unsupported fields must be accepted: %v", err)
	}
}

func TestParseProcessOptional(t *testing.T) {
	start := strings.Index(defaultConfig, `"process"`)
	end := strings.Index(defaultConfig, `"root"`)
	cfg := defaultConfig[:start] + defaultConfig[end:]
	spec, err := ParseSpec([]byte(cfg))
	if err != nil {
		t.Fatalf("a config without process must parse: %v", err)
	}
	if spec.Process != nil {
		t.Fatal("process should be nil")
	}
}

func TestParseMountOptions(t *testing.T) {
	o, err := parseMountOptions([]string{"rbind", "ro", "nosuid", "rw", "rslave", "mode=755", "size=1k"})
	if err != nil {
		t.Fatal(err)
	}
	if want := uintptr(0x1000 | 0x4000 | 0x2); o.Flags != want { // MS_BIND|MS_REC|MS_NOSUID, rw cleared ro
		t.Fatalf("flags = %#x, want %#x", o.Flags, want)
	}
	if len(o.Propagation) != 1 || o.Propagation[0] != 0x80000|0x4000 { // MS_SLAVE|MS_REC
		t.Fatalf("propagation = %#x", o.Propagation)
	}
	if o.Data != "mode=755,size=1k" {
		t.Fatalf("data = %q", o.Data)
	}
	if _, err := parseMountOptions([]string{"rro"}); err == nil {
		t.Fatal("recursive attribute options must be rejected")
	}
}

func TestPlanNamespaces(t *testing.T) {
	l := &Linux{Namespaces: []Namespace{
		{Type: "pid", Path: "/proc/9/ns/pid_for_children"}, {Type: "network"},
		{Type: "mount", Path: "/proc/9/ns/mnt"}, {Type: "uts"}, {Type: "user"},
	}}
	plan := planNamespaces(l)
	if plan.CloneFlags != namespaceFlags["network"]|namespaceFlags["uts"] {
		t.Fatalf("clone flags = %#x", plan.CloneFlags)
	}
	if plan.NewUser {
		t.Fatal("a user namespace without mappings must not be created")
	}
	if plan.PidPath != "/proc/9/ns/pid_for_children" || len(plan.JoinPaths) != 1 || plan.JoinPaths[0].Type != "mount" {
		t.Fatalf("unexpected plan %+v", plan)
	}
	l.UIDMappings = []IDMapping{{ContainerID: 0, HostID: 1000, Size: 1000}}
	plan = planNamespaces(l)
	if !plan.NewUser || plan.CloneFlags&namespaceFlags["user"] == 0 {
		t.Fatalf("user namespace with mappings must be created: %+v", plan)
	}
}

func TestNamespaceTypeCheck(t *testing.T) {
	f, err := openNamespace(Namespace{Type: "network", Path: "/proc/self/ns/net"})
	if err != nil {
		t.Skipf("namespace files not usable here: %v", err)
	}
	f.Close()
	if _, err := openNamespace(Namespace{Type: "uts", Path: "/proc/self/ns/net"}); err == nil {
		t.Fatal("a net namespace file must not pass as uts")
	}
}

func TestCapMask(t *testing.T) {
	m := capMask([]string{"CAP_CHOWN", "CAP_SYS_ADMIN", "CAP_CHECKPOINT_RESTORE"}, 40)
	if m[0] != 1|1<<21 || m[1] != 1<<(40-32) {
		t.Fatalf("mask = %#x %#x", m[0], m[1])
	}
	if m := capMask([]string{"CAP_CHECKPOINT_RESTORE"}, 39); m[1] != 0 {
		t.Fatal("capabilities above the kernel's last one must be skipped")
	}
}
