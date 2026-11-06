package runtime

import (
	"reflect"
	"testing"
)

func TestResolveCgroupPath(t *testing.T) {
	self := []byte("0::/user.slice/anden-x.scope/dc/node1/agent\n")
	cases := []struct {
		path, want string
	}{
		{"/user.slice/anden-x.scope/dc/node1/svc-a-1", "/sys/fs/cgroup/user.slice/anden-x.scope/dc/node1/svc-a-1"},
		{"svc-a-1", "/sys/fs/cgroup/user.slice/anden-x.scope/dc/node1/agent/svc-a-1"},
		{"sub/svc", "/sys/fs/cgroup/user.slice/anden-x.scope/dc/node1/agent/sub/svc"},
	}
	for _, c := range cases {
		got, err := resolveCgroupPath(c.path, self)
		if err != nil || got != c.want {
			t.Errorf("resolveCgroupPath(%q) = %q, %v; want %q", c.path, got, err, c.want)
		}
	}
	for _, bad := range []string{"../escape", ".", "system.slice:anden:x"} {
		if _, err := resolveCgroupPath(bad, self); err == nil {
			t.Errorf("resolveCgroupPath(%q) accepted", bad)
		}
	}
	if _, err := resolveCgroupPath("x", []byte("1:name=systemd:/\n")); err == nil {
		t.Error("a relative path without a cgroup v2 entry must fail")
	}
	got, err := resolveCgroupPath("x", []byte("0::/\n"))
	if err != nil || got != "/sys/fs/cgroup/x" {
		t.Errorf("relative to the root cgroup: %q, %v", got, err)
	}
}

func TestCgroupFiles(t *testing.T) {
	mem, quota, unlimited := int64(64<<20), int64(50000), int64(-1)
	period := uint64(200000)
	pids := int64(32)
	files, controllers := cgroupFiles(&Resources{
		Memory: &Memory{Limit: &mem},
		CPU:    &CPU{Quota: &quota, Period: &period},
		Pids:   &Pids{Limit: &pids},
	})
	want := map[string]string{"memory.max": "67108864", "cpu.max": "50000 200000", "pids.max": "32"}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("files = %v, want %v", files, want)
	}
	if !reflect.DeepEqual(controllers, []string{"memory", "cpu", "pids"}) {
		t.Fatalf("controllers = %v", controllers)
	}

	files, _ = cgroupFiles(&Resources{Memory: &Memory{Limit: &unlimited}, CPU: &CPU{Quota: &unlimited}, Pids: &Pids{Limit: &unlimited}})
	want = map[string]string{"memory.max": "max", "cpu.max": "max 100000", "pids.max": "max"}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("unlimited files = %v, want %v", files, want)
	}

	files, controllers = cgroupFiles(&Resources{Devices: []DeviceRule{{Allow: false, Access: "rwm"}}})
	if len(files) != 0 || len(controllers) != 0 {
		t.Fatalf("device rules alone need no cgroup files: %v %v", files, controllers)
	}
}

func TestPlanCgroup(t *testing.T) {
	self := []byte("0::/a/b\n")
	plan, err := planCgroup(&Linux{}, "c1", self)
	if err != nil || plan != nil {
		t.Fatalf("no path and no limits must leave the caller's cgroup: %+v %v", plan, err)
	}
	limit := int64(10)
	plan, err = planCgroup(&Linux{Resources: &Resources{Pids: &Pids{Limit: &limit}}}, "c1", self)
	if err != nil || plan.Dir != "/sys/fs/cgroup/a/b/anden-runtime-c1" {
		t.Fatalf("limits without a path: %+v %v", plan, err)
	}
	plan, err = planCgroup(&Linux{CgroupsPath: "/x/y"}, "c1", self)
	if err != nil || plan.Dir != "/sys/fs/cgroup/x/y" || len(plan.Files) != 0 {
		t.Fatalf("path without limits: %+v %v", plan, err)
	}
}
