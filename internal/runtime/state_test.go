package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestDeriveStatus(t *testing.T) {
	cases := []struct {
		pid         int
		alive, fifo bool
		want        Status
	}{
		{0, false, false, StatusCreating},
		{0, false, true, StatusCreating},
		{42, true, true, StatusCreated},
		{42, true, false, StatusRunning},
		{42, false, true, StatusStopped},
		{42, false, false, StatusStopped},
	}
	for _, c := range cases {
		if got := deriveStatus(c.pid, c.alive, c.fifo); got != c.want {
			t.Errorf("deriveStatus(%d, %v, %v) = %s, want %s", c.pid, c.alive, c.fifo, got, c.want)
		}
	}
}

func TestTransitions(t *testing.T) {
	all := []Status{StatusCreating, StatusCreated, StatusRunning, StatusStopped}
	for _, s := range all {
		if got, want := canStart(s), s == StatusCreated; got != want {
			t.Errorf("canStart(%s) = %v", s, got)
		}
		if got, want := canKill(s), s == StatusCreated || s == StatusRunning; got != want {
			t.Errorf("canKill(%s) = %v", s, got)
		}
		if got, want := canDelete(s, false), s == StatusCreated || s == StatusStopped; got != want {
			t.Errorf("canDelete(%s) = %v", s, got)
		}
		if !canDelete(s, true) {
			t.Errorf("canDelete(%s, force) = false", s)
		}
	}
}

func TestParseProcStat(t *testing.T) {
	line := "1234 (my (odd) name) S 1 1234 1234 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 987654 1000000 200 18446744073709551615\n"
	st, err := parseProcStat([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if st.State != 'S' || st.StartTime != 987654 {
		t.Fatalf("got %+v", st)
	}
	if _, err := parseProcStat([]byte("1 (x) S 1 2")); err == nil {
		t.Fatal("short stat must fail")
	}
}

func TestProcessAliveSelf(t *testing.T) {
	st, err := readProcStat(os.Getpid())
	if err != nil {
		t.Skipf("no /proc: %v", err)
	}
	if !processAlive(os.Getpid(), st.StartTime) {
		t.Fatal("the test process must be alive")
	}
	if processAlive(os.Getpid(), st.StartTime+1) {
		t.Fatal("a different start time means a reused pid, not the same process")
	}
}

func TestStoreStatusAndState(t *testing.T) {
	s := store{root: t.TempDir()}
	st, err := readProcStat(os.Getpid())
	if err != nil {
		t.Skipf("no /proc: %v", err)
	}
	r := &record{ID: "c1", Bundle: "/b", Annotations: map[string]string{"k": "v"}}
	if err := os.Mkdir(s.dir("c1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.save(r); err != nil {
		t.Fatal(err)
	}
	if got := s.status(r); got != StatusCreating {
		t.Fatalf("status = %s, want creating", got)
	}

	// The test process stands in for a live init waiting on the FIFO.
	r.Pid, r.StartTime = os.Getpid(), st.StartTime
	if err := syscall.Mkfifo(filepath.Join(s.dir("c1"), fifoFile), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := s.status(r); got != StatusCreated {
		t.Fatalf("status = %s, want created", got)
	}
	if err := os.Remove(filepath.Join(s.dir("c1"), fifoFile)); err != nil {
		t.Fatal(err)
	}
	if got := s.status(r); got != StatusRunning {
		t.Fatalf("status = %s, want running", got)
	}

	r.StartTime++
	if err := s.save(r); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.load("c1")
	if err != nil {
		t.Fatal(err)
	}
	state := s.state(loaded)
	if state.Status != StatusStopped || state.Pid != 0 {
		t.Fatalf("state = %+v, want stopped without pid", state)
	}
	out, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"ociVersion":"1.3.0"`, `"id":"c1"`, `"status":"stopped"`, `"bundle":"/b"`, `"annotations":{"k":"v"}`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("state JSON %s lacks %s", out, want)
		}
	}
	if strings.Contains(string(out), `"pid"`) {
		t.Errorf("stopped state JSON %s must not carry a pid", out)
	}
}

func TestStoreLoadMissing(t *testing.T) {
	s := store{root: t.TempDir()}
	if _, err := s.load("nope"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("load of a missing container: %v", err)
	}
	if _, err := s.load("../etc"); err == nil {
		t.Fatal("an id with a path separator must be rejected")
	}
}

func TestLifecycleTransitionErrors(t *testing.T) {
	s := store{root: t.TempDir()}
	if err := os.Mkdir(s.dir("dead"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A pid that cannot belong to a live process with start time 0.
	r := &record{ID: "dead", Pid: os.Getpid(), StartTime: 0, Bundle: "/b"}
	if err := s.save(r); err != nil {
		t.Fatal(err)
	}
	if err := start(s, "dead"); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("start of a stopped container: %v", err)
	}
	if err := kill(s, "dead", syscall.SIGTERM); err == nil {
		t.Fatal("kill of a stopped container must fail")
	}
	if err := remove(s, "dead", false); err != nil {
		t.Fatalf("delete of a stopped container: %v", err)
	}
	if _, err := os.Stat(s.dir("dead")); !os.IsNotExist(err) {
		t.Fatal("delete must remove the state directory")
	}
}

func TestDeleteRunningNeedsForce(t *testing.T) {
	st, err := readProcStat(os.Getpid())
	if err != nil {
		t.Skipf("no /proc: %v", err)
	}
	s := store{root: t.TempDir()}
	if err := os.Mkdir(s.dir("live"), 0o700); err != nil {
		t.Fatal(err)
	}
	// The test process plays a running container; force is never used
	// here because it would kill the test.
	r := &record{ID: "live", Pid: os.Getpid(), StartTime: st.StartTime, Bundle: "/b"}
	if err := s.save(r); err != nil {
		t.Fatal(err)
	}
	if err := remove(s, "live", false); err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("delete of a running container: %v", err)
	}
	if err := start(s, "live"); err == nil {
		t.Fatal("start of a running container must fail")
	}
	if _, err := os.Stat(s.dir("live")); err != nil {
		t.Fatal("a refused delete must leave the container in place")
	}
}
