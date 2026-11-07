package agent

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	andenv1 "github.com/CesarManzoCode/anden/gen/anden/v1"
)

// The shape of `ldd /usr/bin/criu` recorded in vault/RESEARCH.md.
const lddOutput = `	linux-vdso.so.1 (0x00007ffd1a5f2000)
	libprotobuf-c.so.1 => /usr/lib/libprotobuf-c.so.1 (0x00007f0e1c9a0000)
	libnl-3.so.200 => /usr/lib/libnl-3.so.200 (0x00007f0e1c97c000)
	libc.so.6 => /usr/lib/libc.so.6 (0x00007f0e1c600000)
	/lib64/ld-linux-x86-64.so.2 => /usr/lib64/ld-linux-x86-64.so.2 (0x00007f0e1ca1e000)
`

func TestParseLdd(t *testing.T) {
	libs, interp, err := parseLdd(lddOutput)
	if err != nil {
		t.Fatal(err)
	}
	if interp != "ld-linux-x86-64.so.2" {
		t.Errorf("interpreter %q", interp)
	}
	want := []library{
		{name: "libprotobuf-c.so.1", path: "/usr/lib/libprotobuf-c.so.1"},
		{name: "libnl-3.so.200", path: "/usr/lib/libnl-3.so.200"},
		{name: "libc.so.6", path: "/usr/lib/libc.so.6"},
		{name: "ld-linux-x86-64.so.2", path: "/usr/lib64/ld-linux-x86-64.so.2"},
	}
	if !reflect.DeepEqual(libs, want) {
		t.Errorf("libraries %+v", libs)
	}
	if _, _, err := parseLdd("\tlibfoo.so.1 => not found\n" + lddOutput); err == nil {
		t.Error("a missing library must fail")
	}
	if _, _, err := parseLdd("\tnot a dynamic executable\n"); err == nil {
		t.Error("without a loader there is nothing to run criu with")
	}
}

func TestDumpArgv(t *testing.T) {
	got := dumpArgv("ld-linux-x86-64.so.2", 4242, 4026532410)
	want := []string{
		"/tmp/c/ld-linux-x86-64.so.2", "--library-path", "/tmp/c", "/tmp/c/criu",
		"dump", "--network-lock", "skip", "--unprivileged", "--manage-cgroups=ignore",
		"-t", "4242", "-D", "/tmp/img", "--root", "/",
		"--external", "net[4026532410]:extnet",
		"-o", "dump.log", "-v2",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dump argv\n got %q\nwant %q", got, want)
	}
}

func TestRestoreArgv(t *testing.T) {
	got := restoreArgv("/usr/bin/criu", "/s/snapshots/echo-1", "/s/instances/echo-1/merged", "/s/snapshots/echo-1/restore.pid")
	want := []string{
		"/usr/bin/criu", "restore", "--network-lock", "skip", "--unprivileged", "--manage-cgroups=ignore",
		"-D", "/s/snapshots/echo-1", "--root", "/s/instances/echo-1/merged",
		"--inherit-fd", "fd[3]:extnet",
		"-d", "--pidfile", "/s/snapshots/echo-1/restore.pid", "-o", "restore.log", "-v2",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("restore argv\n got %q\nwant %q", got, want)
	}
}

func TestDumpView(t *testing.T) {
	m := "/s/instances/echo-1/merged"
	libs := []library{
		{name: "libc.so.6", path: "/usr/lib/libc.so.6"},
		{name: "ld-linux-x86-64.so.2", path: "/usr/lib64/ld-linux-x86-64.so.2"},
	}
	got := dumpView(m, "/s/snapshots/echo-1", "/usr/bin/criu", libs)
	want := []step{
		{Op: opPrivate, Target: "/"},
		{Op: opBind, Source: m, Target: m},
		{Op: opMkdir, Target: m + "/tmp"},
		{Op: opTmpfs, Target: m + "/tmp"},
		{Op: opMkdir, Target: m + "/proc"},
		{Op: opProc, Target: m + "/proc"},
		{Op: opMkdir, Target: m + "/dev"},
		{Op: opTouch, Target: m + "/dev/null"},
		{Op: opBind, Source: "/dev/null", Target: m + "/dev/null"},
		{Op: opMkdir, Target: m + "/tmp/img"},
		{Op: opBind, Source: "/s/snapshots/echo-1", Target: m + "/tmp/img"},
		{Op: opMkdir, Target: m + "/tmp/c"},
		{Op: opCopy, Source: "/usr/bin/criu", Target: m + "/tmp/c/criu"},
		{Op: opCopy, Source: "/usr/lib/libc.so.6", Target: m + "/tmp/c/libc.so.6"},
		{Op: opCopy, Source: "/usr/lib64/ld-linux-x86-64.so.2", Target: m + "/tmp/c/ld-linux-x86-64.so.2"},
		{Op: opChroot, Target: m},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dump view\n got %+v\nwant %+v", got, want)
	}
	// The tmpfs must be there before anything is put in /tmp, and the chroot
	// comes last because the sources are host paths.
	index := func(op, target string) int {
		for i, s := range got {
			if s.Op == op && s.Target == target {
				return i
			}
		}
		return -1
	}
	if index(opTmpfs, m+"/tmp") > index(opBind, m+"/tmp/img") || index(opTmpfs, m+"/tmp") > index(opCopy, m+"/tmp/c/criu") {
		t.Error("the tmpfs on /tmp must come before what goes in it")
	}
	if got[len(got)-1].Op != opChroot {
		t.Error("the chroot must be the last step")
	}
}

func TestRestoreView(t *testing.T) {
	m := "/s/instances/echo-1/merged"
	if got := restoreView(m); !reflect.DeepEqual(got, []step{{Op: opPrivate, Target: "/"}, {Op: opBind, Source: m, Target: m}}) {
		t.Errorf("restore view %+v", got)
	}
}

func TestRequestedRoute(t *testing.T) {
	for route, want := range map[andenv1.WakeRoute]string{
		andenv1.WakeRoute_WAKE_ROUTE_UNSPECIFIED: routeRelaunch,
		andenv1.WakeRoute_WAKE_ROUTE_RELAUNCH:    routeRelaunch,
		andenv1.WakeRoute_WAKE_ROUTE_RESTORE:     routeRestore,
	} {
		if got := requestedRoute(&andenv1.ServiceSpec{WakeRoute: route}); got != want {
			t.Errorf("%v: %q, want %q", route, got, want)
		}
	}
}

func TestWakeByFallsBackToRelaunch(t *testing.T) {
	failed := errors.New("criu restore failed")
	for _, c := range []struct {
		name         string
		requested    string
		snapshot     bool
		restoreErr   error
		wantRoute    string
		wantRestore  bool
		wantRelaunch bool
	}{
		{"restored", routeRestore, true, nil, routeRestore, true, false},
		{"restore fails", routeRestore, true, failed, routeRelaunch, true, true},
		{"no snapshot", routeRestore, false, nil, routeRelaunch, false, true},
		{"relaunch asked", routeRelaunch, true, nil, routeRelaunch, false, true},
	} {
		restored, relaunched := false, false
		route, err := wakeBy(c.requested, c.snapshot,
			func() error { restored = true; return c.restoreErr },
			func() error { relaunched = true; return nil })
		if err != nil || route != c.wantRoute || restored != c.wantRestore || relaunched != c.wantRelaunch {
			t.Errorf("%s: route %q, err %v, restore %v, relaunch %v", c.name, route, err, restored, relaunched)
		}
	}
	// A failed relaunch is the error of the wake, after a failed restore too.
	broken := errors.New("anden-runtime create failed")
	route, err := wakeBy(routeRestore, true, func() error { return failed }, func() error { return broken })
	if route != routeRelaunch || !errors.Is(err, broken) {
		t.Errorf("route %q, err %v", route, err)
	}
}

func TestSnapshotPath(t *testing.T) {
	p := instancePaths("/s/nodes/node1", "/sys/fs/cgroup/x/dc/node1", "echo", 3, "echo-3")
	if p.snapshot != "/s/nodes/node1/snapshots/echo-3" {
		t.Fatalf("snapshot %q", p.snapshot)
	}
}

func TestViewStepsRefuseSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "tmp")); err != nil {
		t.Fatal(err)
	}
	if err := applyStep(root, step{Op: opMkdir, Target: filepath.Join(root, "tmp", "c")}); err == nil {
		t.Error("a target through a symlink of the rootfs must be refused")
	}
	if _, err := os.Stat(filepath.Join(outside, "c")); err == nil {
		t.Error("the step went out of the rootfs")
	}

	if err := applyStep(root, step{Op: opMkdir, Target: filepath.Join(root, "dev")}); err != nil {
		t.Fatal(err)
	}
	null := filepath.Join(root, "dev", "null")
	for range 2 {
		if err := applyStep(root, step{Op: opTouch, Target: null}); err != nil {
			t.Fatalf("touch: %v", err)
		}
	}
	src := filepath.Join(outside, "criu")
	if err := os.WriteFile(src, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(root, "dev", "criu")
	if err := applyStep(root, step{Op: opCopy, Source: src, Target: dst}); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(dst); err != nil || string(raw) != "binary" {
		t.Fatalf("copy %q, %v", raw, err)
	}
	if err := applyStep(root, step{Op: "remount", Target: root}); err == nil {
		t.Error("an unknown step must fail")
	}
}
