package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func fakeProc(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "self"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "4242"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "4242", "comm"), []byte("a-very-long-pro\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestHostProcVisible(t *testing.T) {
	root := fakeProc(t)
	cases := []struct {
		name string
		pid  string
		comm string
		want bool
	}{
		{"same comm", "4242", "a-very-long-pro", true},
		{"comm the kernel truncated", "4242", "a-very-long-process-name", true},
		{"same pid with another comm", "4242", "sleep", false},
		{"pid absent", "4243", "a-very-long-pro", false},
	}
	for _, c := range cases {
		got, err := hostProcVisible(root, c.pid, c.comm)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestHostProcVisibleRefusesWhatItCannotCheck(t *testing.T) {
	root := fakeProc(t)
	if _, err := hostProcVisible(root, "", "sleep"); err == nil {
		t.Error("empty PID was accepted")
	}
	if _, err := hostProcVisible(root, "../self", "sleep"); err == nil {
		t.Error("a path was accepted as a PID")
	}
	if _, err := hostProcVisible(root, "4242", ""); err == nil {
		t.Error("empty comm was accepted")
	}
	if _, err := hostProcVisible(t.TempDir(), "4242", "sleep"); err == nil {
		t.Error("a proc without self was accepted")
	}
}

func TestSyscallOutcome(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, "OK"},
		{syscall.EPERM, "EPERM"},
		{syscall.ENOENT, "ENOENT"},
		{syscall.EBUSY, "errno 16"},
		{errors.New("other"), "other"},
	}
	for _, c := range cases {
		if got := syscallOutcome(c.err); got != c.want {
			t.Errorf("%v: got %q, want %q", c.err, got, c.want)
		}
	}
}

func TestCollect(t *testing.T) {
	root := fakeProc(t)
	own, err := nodename()
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"PROBE_HOST_HOSTNAME": "not-" + own, "PROBE_HOST_PID": "4243", "PROBE_HOST_COMM": "sleep"}
	getenv := func(key string) string { return env[key] }

	rep, err := collect(getenv, root)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.HostnameOwn {
		t.Error("a different host hostname must report hostname_own")
	}
	if rep.HostProcsVisible {
		t.Error("an absent PID must not report host_procs_visible")
	}
	for _, name := range []string{"mount", "swapon"} {
		if rep.BlockedSyscalls[name] == "OK" {
			t.Errorf("%s succeeded on a path that does not exist", name)
		}
	}

	env["PROBE_HOST_HOSTNAME"] = own
	if rep, err = collect(getenv, root); err != nil {
		t.Fatal(err)
	}
	if rep.HostnameOwn {
		t.Error("the same hostname must not report hostname_own")
	}

	delete(env, "PROBE_HOST_HOSTNAME")
	if _, err = collect(getenv, root); err == nil {
		t.Error("a missing PROBE_HOST_HOSTNAME was accepted")
	}
}

func TestReportKeysMatchTheContract(t *testing.T) {
	data, err := json.Marshal(report{BlockedSyscalls: map[string]string{"mount": "EPERM"}})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"pid1", "hostname_own", "host_procs_visible", "blocked_syscalls"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("report lacks %s: %s", key, data)
		}
	}
}
