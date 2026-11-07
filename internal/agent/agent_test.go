package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	andenv1 "github.com/CesarManzoCode/anden/gen/anden/v1"
	"github.com/CesarManzoCode/anden/internal/image"
	ociruntime "github.com/CesarManzoCode/anden/internal/runtime"
)

func testSpec() *andenv1.ServiceSpec {
	return &andenv1.ServiceSpec{Name: "echo", Port: 8080, CpuMillis: 500, MemoryMib: 64, PidsMax: 32,
		Env: map[string]string{"VERSION": "v1", "B": "2"}, IdleSeconds: proto.Uint32(0)}
}

func testInput() configInput {
	return configInput{
		spec:   testSpec(),
		run:    image.RunConfig{Cmd: []string{"/usr/bin/echosvc"}, Env: []string{"PATH=/bin", "VERSION=image", "A=1"}},
		rootfs: "/s/instances/echo-1/merged",
		netNS:  "/s/instances/echo-1/net.ns",
		utsNS:  "/s/instances/echo-1/uts.ns",
		ipcNS:  "/s/instances/echo-1/ipc.ns",
		cgroup: "/sys/fs/cgroup/x/anden-h.scope/dc/node1/svc-echo-1",
	}
}

func parseConfig(t *testing.T, in configInput) *ociruntime.Spec {
	t.Helper()
	config, err := buildConfig(in)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	// What the runtime itself accepts is the test of validity.
	parsed, err := ociruntime.ParseSpec(raw)
	if err != nil {
		t.Fatalf("anden-runtime refuses the config: %v\n%s", err, raw)
	}
	return parsed
}

func TestBuildConfigLimitsAndNamespaces(t *testing.T) {
	c := parseConfig(t, testInput())
	r := c.Linux.Resources
	if *r.Memory.Limit != 64<<20 {
		t.Errorf("memory limit %d", *r.Memory.Limit)
	}
	if *r.CPU.Quota != 50000 || *r.CPU.Period != 100000 {
		t.Errorf("cpu %d / %d", *r.CPU.Quota, *r.CPU.Period)
	}
	if *r.Pids.Limit != 32 {
		t.Errorf("pids %d", *r.Pids.Limit)
	}
	if c.Linux.CgroupsPath != "/x/anden-h.scope/dc/node1/svc-echo-1" {
		t.Errorf("cgroupsPath %s", c.Linux.CgroupsPath)
	}
	want := []ociruntime.Namespace{{Type: "pid"}, {Type: "mount"}, {Type: "network", Path: "/s/instances/echo-1/net.ns"},
		{Type: "uts", Path: "/s/instances/echo-1/uts.ns"}, {Type: "ipc", Path: "/s/instances/echo-1/ipc.ns"}}
	if !reflect.DeepEqual(c.Linux.Namespaces, want) {
		t.Errorf("namespaces %+v", c.Linux.Namespaces)
	}
	if c.Root.Path != "/s/instances/echo-1/merged" || c.Hostname != "echo" || !c.Process.NoNewPrivileges || c.Process.Cwd != "/" {
		t.Errorf("config %+v %+v", c.Root, c.Process)
	}
	if slices.Contains(c.Process.Capabilities.Bounding, "CAP_SYS_ADMIN") {
		t.Error("an instance must not keep CAP_SYS_ADMIN")
	}
}

func TestBuildConfigWithoutPidsLimit(t *testing.T) {
	in := testInput()
	in.spec.PidsMax = 0
	if c := parseConfig(t, in); c.Linux.Resources.Pids != nil {
		t.Errorf("pids_max 0 means no limit, got %+v", c.Linux.Resources.Pids)
	}
}

func TestBuildConfigSeccompByRoute(t *testing.T) {
	c := parseConfig(t, testInput())
	if c.Linux.Seccomp == nil || !slices.Contains(c.Linux.Seccomp.Syscalls[0].Names, "mount") {
		t.Fatalf("relaunch needs its filter: %+v", c.Linux.Seccomp)
	}
	in := testInput()
	in.spec.WakeRoute = andenv1.WakeRoute_WAKE_ROUTE_RESTORE
	if c := parseConfig(t, in); c.Linux.Seccomp != nil {
		t.Errorf("restore runs without a filter (D19): %+v", c.Linux.Seccomp)
	}
	if seccompEnabled(in.spec) || !seccompEnabled(testSpec()) {
		t.Error("seccompEnabled does not follow the route")
	}
}

func TestProcessArgs(t *testing.T) {
	run := image.RunConfig{Entrypoint: []string{"/e"}, Cmd: []string{"c"}}
	if got, _ := processArgs(run, nil); !reflect.DeepEqual(got, []string{"/e", "c"}) {
		t.Errorf("entrypoint and cmd: %q", got)
	}
	if got, _ := processArgs(run, []string{"a"}); !reflect.DeepEqual(got, []string{"/e", "a"}) {
		t.Errorf("args replace cmd: %q", got)
	}
	if _, err := processArgs(image.RunConfig{}, nil); err == nil {
		t.Error("nothing to run must fail")
	}
}

func TestProcessEnv(t *testing.T) {
	run := image.RunConfig{Env: []string{"PATH=/bin", "VERSION=image", "A=1"}}
	got := processEnv(run, map[string]string{"VERSION": "v1", "B": "2"})
	if want := []string{"PATH=/bin", "A=1", "B=2", "VERSION=v1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("env %q, want %q", got, want)
	}
	if got := processEnv(image.RunConfig{}, nil); !reflect.DeepEqual(got, []string{defaultPath}) {
		t.Errorf("default PATH: %q", got)
	}
}

func TestNetworkCommands(t *testing.T) {
	host, peer := vethNames("a-service-with-a-very-long-name-12")
	if len(host) > 15 || len(peer) > 15 || host == peer {
		t.Fatalf("veth names %q %q", host, peer)
	}
	if other, _ := vethNames("another-1"); other == host {
		t.Fatal("two instances share a veth name")
	}
	if got := hostBatch("vh1", "vc1", "/s/net.ns"); !reflect.DeepEqual(got, []string{
		"link add vh1 type veth peer name vc1",
		"link set vc1 netns /s/net.ns",
		"link set vh1 master svc0",
		"link set vh1 up",
	}) {
		t.Errorf("hostBatch %q", got)
	}
	if got := instanceBatch("vc1", "10.81.0.2", 16, "10.81.0.1"); !reflect.DeepEqual(got, []string{
		"link set lo up",
		"link set vc1 name eth0",
		"addr add 10.81.0.2/16 dev eth0",
		"link set eth0 up",
		"route add default via 10.81.0.1",
	}) {
		t.Errorf("instanceBatch %q", got)
	}
	if got := namespaceArgs("/n", "/u", "/i"); !reflect.DeepEqual(got, []string{"--net=/n", "--uts=/u", "--ipc=/i", "true"}) {
		t.Errorf("namespaceArgs %q", got)
	}
}

func assignment(service string, generation uint64, awake bool) *andenv1.Assignment {
	return &andenv1.Assignment{
		InstanceId: service + "-" + strconv.FormatUint(generation, 10),
		Spec:       &andenv1.ServiceSpec{Name: service},
		Generation: generation,
		Awake:      awake,
	}
}

func TestDiff(t *testing.T) {
	cases := []struct {
		name      string
		desired   []*andenv1.Assignment
		have      map[string]local
		wantStart []string
		wantStop  []string
	}{
		{"nothing", nil, nil, nil, nil},
		{"new assignment starts", []*andenv1.Assignment{assignment("a", 1, true)}, nil, []string{"a-1"}, nil},
		{"present stays", []*andenv1.Assignment{assignment("a", 1, true)}, map[string]local{"a-1": {service: "a"}}, nil, nil},
		{"asleep does not start", []*andenv1.Assignment{assignment("a", 1, false)}, nil, nil, nil},
		{"sleep stops", []*andenv1.Assignment{assignment("a", 1, false)}, map[string]local{"a-1": {service: "a"}}, nil, []string{"a-1"}},
		{"unassigned stops", nil, map[string]local{"a-1": {service: "a"}}, nil, []string{"a-1"}},
		{"stopping is not stopped twice", nil, map[string]local{"a-1": {service: "a", stopping: true}}, nil, nil},
		{"new generation waits for the old to go", []*andenv1.Assignment{assignment("a", 2, true)},
			map[string]local{"a-1": {service: "a"}}, nil, []string{"a-1"}},
		{"new generation still waits while it stops", []*andenv1.Assignment{assignment("a", 2, true)},
			map[string]local{"a-1": {service: "a", stopping: true}}, nil, nil},
		{"other services do not interfere", []*andenv1.Assignment{assignment("a", 1, true), assignment("b", 1, true)},
			map[string]local{"a-1": {service: "a"}}, []string{"b-1"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			start, stop := diff(c.desired, c.have)
			var ids []string
			for _, as := range start {
				ids = append(ids, as.GetInstanceId())
			}
			if !slices.Equal(ids, c.wantStart) || !slices.Equal(stop, c.wantStop) {
				t.Fatalf("start %q stop %q, want %q %q", ids, stop, c.wantStart, c.wantStop)
			}
		})
	}
}

func TestInstancePaths(t *testing.T) {
	p := instancePaths("/s/nodes/node1", "/sys/fs/cgroup/x/dc/node1", "echo", 3, "echo-3")
	if p.dir != filepath.Join("/s/nodes/node1", "instances", "echo-3") || !strings.HasSuffix(p.merged, "echo-3/merged") ||
		!strings.HasSuffix(p.netNS, "echo-3/net.ns") || p.cgroup != "/sys/fs/cgroup/x/dc/node1/svc-echo-3" {
		t.Fatalf("%+v", p)
	}
}

func TestParsePopulated(t *testing.T) {
	if !parsePopulated([]byte("populated 1\nfrozen 0\n")) || parsePopulated([]byte("populated 0\nfrozen 0\n")) {
		t.Fatal("populated misread")
	}
}

func TestPrepareRootfsDoesNotFollowLinks(t *testing.T) {
	merged := t.TempDir()
	stdio, err := prepareRootfs(merged, "10.80.0.1")
	if err != nil {
		t.Fatal(err)
	}
	stdio.Close()
	raw, err := os.ReadFile(filepath.Join(merged, "etc", "resolv.conf"))
	if err != nil || string(raw) != "nameserver 10.80.0.1\n" {
		t.Fatalf("resolv.conf %q, %v", raw, err)
	}

	outside := t.TempDir()
	other := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(other, "etc")); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareRootfs(other, "10.80.0.1"); err == nil {
		t.Fatal("/etc as a symlink must be refused")
	}
}
