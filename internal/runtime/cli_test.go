package runtime

import (
	"bytes"
	"strings"
	"syscall"
	"testing"
)

func TestParseArgsOrder(t *testing.T) {
	// runtime-tools: create --pid-file P --bundle B ID
	flags, pos, err := parseArgs([]string{"--pid-file", "/p", "--bundle=/b", "id1"}, createFlags)
	if err != nil {
		t.Fatal(err)
	}
	if flags["pid-file"] != "/p" || flags["bundle"] != "/b" || len(pos) != 1 || pos[0] != "id1" {
		t.Fatalf("flags %v pos %v", flags, pos)
	}
	// Options after the ID work too, with short spellings.
	flags, pos, err = parseArgs([]string{"id1", "-b", "/b"}, createFlags)
	if err != nil || flags["bundle"] != "/b" || pos[0] != "id1" {
		t.Fatalf("flags %v pos %v err %v", flags, pos, err)
	}
	flags, pos, err = parseArgs([]string{"--force", "id1"}, deleteFlags)
	if err != nil || flags["force"] != "true" || pos[0] != "id1" {
		t.Fatalf("flags %v pos %v err %v", flags, pos, err)
	}
	for _, bad := range [][]string{{"--bogus", "x"}, {"--bundle"}, {"--force=yes"}} {
		defs := createFlags
		if bad[0] == "--force=yes" {
			defs = deleteFlags
		}
		if _, _, err := parseArgs(bad, defs); err == nil {
			t.Errorf("parseArgs(%q) accepted", bad)
		}
	}
}

func TestSplitGlobal(t *testing.T) {
	g, cmd, rest, err := splitGlobal([]string{"--root", "/r", "kill", "id", "KILL"})
	if err != nil || g["root"] != "/r" || cmd != "kill" || len(rest) != 2 {
		t.Fatalf("%v %q %v %v", g, cmd, rest, err)
	}
	g, cmd, _, err = splitGlobal([]string{"--root=/r", "state", "id"})
	if err != nil || g["root"] != "/r" || cmd != "state" {
		t.Fatalf("%v %q %v", g, cmd, err)
	}
	for _, bad := range [][]string{{}, {"--root"}, {"--root", "/r"}, {"--debug", "state", "x"}} {
		if _, _, _, err := splitGlobal(bad); err == nil {
			t.Errorf("splitGlobal(%q) accepted", bad)
		}
	}
}

func TestParseSignal(t *testing.T) {
	cases := map[string]syscall.Signal{
		"TERM": syscall.SIGTERM, "KILL": syscall.SIGKILL, "SIGUSR1": syscall.SIGUSR1,
		"usr2": syscall.SIGUSR2, "9": syscall.SIGKILL, "15": syscall.SIGTERM,
	}
	for in, want := range cases {
		got, err := parseSignal(in)
		if err != nil || got != want {
			t.Errorf("parseSignal(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "NOPE", "0", "65", "-1"} {
		if _, err := parseSignal(bad); err == nil {
			t.Errorf("parseSignal(%q) accepted", bad)
		}
	}
}

func TestMainErrors(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--root", root, "start"}, "container id is required"},
		{[]string{"--root", root, "state"}, "container id is required"},
		{[]string{"--root", root, "kill"}, "container id is required"},
		{[]string{"--root", root, "delete"}, "container id is required"},
		{[]string{"--root", root, "create", "--bundle", root}, "container id is required"},
		{[]string{"--root", root, "state", "ghost"}, "does not exist"},
		{[]string{"--root", root, "start", "ghost"}, "does not exist"},
		{[]string{"--root", root, "kill", "ghost", "KILL"}, "does not exist"},
		{[]string{"--root", root, "kill", "--signal", "KILL", "ghost", "TERM"}, "both"},
		{[]string{"--root", root, "delete", "ghost"}, "does not exist"},
		{[]string{"--root", root, "create", "--bundle", root, "c1"}, "config.json"},
		{[]string{"--root", root, "frobnicate", "x"}, "unknown command"},
	}
	for _, c := range cases {
		var out, errb bytes.Buffer
		if code := Main(c.args, &out, &errb); code == 0 {
			t.Errorf("Main(%q) exited 0", c.args)
		}
		if !strings.Contains(errb.String(), c.want) {
			t.Errorf("Main(%q) stderr %q lacks %q", c.args, errb.String(), c.want)
		}
	}
}
