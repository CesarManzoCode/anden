package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSecurePath(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"usr/bin", "etc"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	links := map[string]string{
		"bin":        "usr/bin",
		"abs":        "/etc",
		"escape":     "../../../../etc",
		"etc/loop":   "loop",
		"etc/parent": "..",
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]string{
		"/bin/sh":           "usr/bin/sh",
		"/abs/passwd":       "etc/passwd",
		"/escape/passwd":    "etc/passwd",
		"/../../etc":        "etc",
		"/etc/parent/bin":   "usr/bin",
		"/missing/../a/b":   "a/b",
		"/missing/../../..": "",
		"/":                 "",
	}
	for in, want := range cases {
		got, err := securePath(root, in)
		if err != nil {
			t.Errorf("securePath(%q): %v", in, err)
			continue
		}
		if got != filepath.Join(root, want) {
			t.Errorf("securePath(%q) = %q, want %q", in, got, filepath.Join(root, want))
		}
	}
	if _, err := securePath(root, "/etc/loop"); err == nil {
		t.Error("a symlink loop must fail")
	}
	rel, err := securePath(".", "/dev/pts")
	if err != nil || rel != "dev/pts" {
		t.Errorf("relative root: %q, %v", rel, err)
	}
}

func TestMkdev(t *testing.T) {
	cases := []struct {
		major, minor int64
		want         uint64
	}{
		{1, 3, 0x103},      // /dev/null
		{5, 2, 0x502},      // /dev/ptmx
		{259, 0, 0x10300},  // a block device above 255
		{1, 300, 0x10012c}, // minor bits above 8 go to bits 20 and up
	}
	for _, c := range cases {
		if got := mkdev(c.major, c.minor); got != c.want {
			t.Errorf("mkdev(%d, %d) = %#x, want %#x", c.major, c.minor, got, c.want)
		}
	}
}

func TestLookPath(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "tool")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := lookPath("tool", []string{"PATH=/nonexistent:" + dir})
	if err != nil || got != exe {
		t.Fatalf("lookPath = %q, %v", got, err)
	}
	if _, err := lookPath("data", []string{"PATH=" + dir}); err == nil {
		t.Fatal("a file without execute bits must not be found")
	}
	if got, _ := lookPath("/abs/x", nil); got != "/abs/x" {
		t.Fatalf("an absolute path is used as is, got %q", got)
	}
}
