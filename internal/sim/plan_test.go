package sim

import (
	"crypto/sha1"
	"encoding/hex"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestHashIsSha1PrefixOfAbsolutePath(t *testing.T) {
	sum := sha1.Sum([]byte("/home/u/anden/.run/dc"))
	want := hex.EncodeToString(sum[:])[:12]
	if got := Hash("/home/u/anden/.run/dc"); got != want {
		t.Fatalf("Hash = %q, want %q", got, want)
	}
	if len(want) != 12 {
		t.Fatalf("hash length %d", len(want))
	}
	if Hash("/a") == Hash("/b") {
		t.Fatal("distinct paths share a hash")
	}
}

func TestStatePathMakesTheHashIndependentOfTheCwdForm(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	abs, h, err := StatePath(".run/dc")
	if err != nil {
		t.Fatal(err)
	}
	if abs != filepath.Join(dir, ".run/dc") {
		t.Fatalf("abs = %q", abs)
	}
	_, h2, _ := StatePath(filepath.Join(dir, ".run", "dc"))
	if h != h2 || h != Hash(abs) {
		t.Fatalf("hashes differ: %q %q", h, h2)
	}
	if Unit(h) != "anden-"+h {
		t.Fatalf("unit %q", Unit(h))
	}
}

func TestSocketDir(t *testing.T) {
	if got := SocketDir("0123456789ab", "/run/user/1000"); got != "/run/user/1000/anden-0123456789ab" {
		t.Fatalf("with XDG_RUNTIME_DIR: %q", got)
	}
	if got := SocketDir("0123456789ab", ""); got != "/tmp/anden-0123456789ab" {
		t.Fatalf("without XDG_RUNTIME_DIR: %q", got)
	}
	// sun_path holds 107 bytes plus the terminator.
	if p := APISocket(SocketDir("0123456789ab", "/run/user/1000")); len(p) > 107 {
		t.Fatalf("socket path too long: %q", p)
	}
	if p := filepath.Join(SocketDir("0123456789ab", "/run/user/1000"), ".s.PGSQL.5432"); len(p) > 107 {
		t.Fatalf("postgres socket path too long: %q", p)
	}
}

func TestAddressPlan(t *testing.T) {
	want := []Node{
		{Name: "node1", IP: "10.80.0.11", Subnet: "10.81.0.0/16", Gateway: "10.81.0.1", RootVeth: "veth-node1"},
		{Name: "node2", IP: "10.80.0.12", Subnet: "10.82.0.0/16", Gateway: "10.82.0.1", RootVeth: "veth-node2"},
		{Name: "node3", IP: "10.80.0.13", Subnet: "10.83.0.0/16", Gateway: "10.83.0.1", RootVeth: "veth-node3"},
	}
	if got := Nodes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Nodes = %+v", got)
	}
	if _, ok := FindNode("node4"); ok {
		t.Fatal("node4 exists")
	}
	if RootIP != "10.80.0.1" || EdgeIP != "10.80.0.2" || ClientIP != "10.80.0.100" {
		t.Fatal("root, edge or client address changed")
	}
}

func TestRootLinkBatch(t *testing.T) {
	pids := map[string]int{"client": 10, "node1": 11, "node2": 12, "node3": 13}
	lines := RootLinkBatch(pids)
	for _, want := range []string{
		"link add anden0 type bridge",
		"addr add 10.80.0.1/24 dev anden0",
		"addr add 10.80.0.2/32 dev anden0",
		"link set anden0 up",
		"link add veth-client type veth peer name vp-client",
		"link set vp-client netns 10",
		"link add veth-node2 type veth peer name vp-node2",
		"link set vp-node2 netns 12",
		"link set veth-node3 master anden0",
		"link set veth-node3 up",
	} {
		if !contains(lines, want) {
			t.Errorf("missing %q in\n%s", want, strings.Join(lines, "\n"))
		}
	}
	// A veth must exist before it moves or joins the bridge.
	if index(lines, "link add veth-node1 type veth peer name vp-node1") > index(lines, "link set vp-node1 netns 11") {
		t.Error("veth moved before it is created")
	}
	if index(lines, "link add anden0 type bridge") > index(lines, "link set veth-node1 master anden0") {
		t.Error("veth enslaved before the bridge exists")
	}
}

func TestRootRouteBatch(t *testing.T) {
	want := []string{
		"route add 10.81.0.0/16 via 10.80.0.11",
		"route add 10.82.0.0/16 via 10.80.0.12",
		"route add 10.83.0.0/16 via 10.80.0.13",
	}
	if got := RootRouteBatch(); !reflect.DeepEqual(got, want) {
		t.Fatalf("RootRouteBatch = %q", got)
	}
}

func TestPeerBatch(t *testing.T) {
	node, err := PeerBatch("node2")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"link set lo up",
		"link set vp-node2 name eth0",
		"addr add 10.80.0.12/24 dev eth0",
		"link set eth0 up",
		"route add default via 10.80.0.1",
		"link add svc0 type bridge",
		"addr add 10.82.0.1/16 dev svc0",
		"link set svc0 up",
		"route add 10.80.0.100/32 via 10.80.0.1",
	}
	if !reflect.DeepEqual(node, want) {
		t.Fatalf("node2 batch = %q", node)
	}
	client, err := PeerBatch("client")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(client, "addr add 10.80.0.100/24 dev eth0") || contains(client, "link add svc0 type bridge") {
		t.Fatalf("client batch = %q", client)
	}
	if _, err := PeerBatch("node9"); err == nil {
		t.Fatal("unknown peer accepted")
	}
}

func TestCommandLines(t *testing.T) {
	n, _ := FindNode("node1")
	got := AgentArgs(n, "/s", "/sys/fs/cgroup/x/anden-h.scope/dc", "/b")
	want := []string{"--node", "node1", "--addr", "10.80.0.11", "--control", "10.80.0.1:7443",
		"--state", "/s/nodes/node1", "--cgroup", "/sys/fs/cgroup/x/anden-h.scope/dc/node1",
		"--subnet", "10.81.0.0/16", "--runtime", "/b/anden-runtime"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AgentArgs = %q", got)
	}
	if got := AndendArgs("/s", "/run/user/1000/anden-h"); !reflect.DeepEqual(got, []string{
		"--api-socket", "/run/user/1000/anden-h/api.sock", "--listen", "10.80.0.1:7443", "--pg-socket-dir", "/run/user/1000/anden-h",
		"--registry", "/s/registry", "--blobs-listen", "10.80.0.1:7444", "--edge-ip", "10.80.0.2",
		"--dns-listen", "10.80.0.1:53"}) {
		t.Fatalf("AndendArgs = %q", got)
	}
	if got := ExecArgs(42, []string{"nft", "list", "tables"}); !reflect.DeepEqual(got, []string{
		"--preserve-credentials", "-U", "-n", "-t", "42", "--", "nft", "list", "tables"}) {
		t.Fatalf("ExecArgs = %q", got)
	}
}

func TestNodeCapacity(t *testing.T) {
	total, err := parseMemTotal([]byte("Other: 1 kB\nMemTotal:       14680064 kB\nMemFree: 2 kB\n"))
	if err != nil || total != 14680064 {
		t.Fatalf("parseMemTotal = %d, %v", total, err)
	}
	if _, err := parseMemTotal([]byte("MemFree: 2 kB\n")); err == nil {
		t.Fatal("meminfo without MemTotal must fail")
	}
	cpu, mem := NodeCapacity(12, total)
	if cpu != 4000 || mem != 4778 {
		t.Fatalf("NodeCapacity = %d, %d", cpu, mem)
	}
}

func TestCgroupDirAndNSpid(t *testing.T) {
	dir, err := cgroupDir([]byte("0::/user.slice/user-1000.slice/user@1000.service/app.slice/anden-h.scope\n"))
	if err != nil || dir != "/sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice/anden-h.scope" {
		t.Fatalf("cgroupDir = %q, %v", dir, err)
	}
	if _, err := cgroupDir([]byte("1:name=systemd:/\n")); err == nil {
		t.Fatal("cgroup v1 line accepted")
	}
	pids, err := parseNSpid([]byte("Name:\tsleep\nNSpid:\t48211\t7\nPPid:\t1\n"))
	if err != nil || !reflect.DeepEqual(pids, []int{48211, 7}) {
		t.Fatalf("parseNSpid = %v, %v", pids, err)
	}
	if _, err := parseNSpid([]byte("Name:\tx\n")); err == nil {
		t.Fatal("status without NSpid accepted")
	}
}

func contains(lines []string, s string) bool { return index(lines, s) >= 0 }

func index(lines []string, s string) int {
	for i, l := range lines {
		if l == s {
			return i
		}
	}
	return -1
}

func TestChaosSteps(t *testing.T) {
	n, _ := FindNode("node2")
	cases := []struct {
		action  string
		delayMs int
		want    []ChaosStep
	}{
		{"partition", 0, []ChaosStep{{Cmd: []string{"ip", "link", "set", "veth-node2", "down"}}}},
		{"heal", 0, []ChaosStep{
			{Cmd: []string{"tc", "qdisc", "del", "dev", "veth-node2", "root"}, Optional: true},
			{Cmd: []string{"ip", "link", "set", "veth-node2", "up"}},
		}},
		{"slow", 150, []ChaosStep{{Cmd: []string{"tc", "qdisc", "replace", "dev", "veth-node2", "root", "netem", "delay", "150ms"}}}},
	}
	for _, c := range cases {
		got, err := ChaosSteps(c.action, n, c.delayMs)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %v, %v", c.action, got, err)
		}
	}
	for _, bad := range []struct {
		action  string
		delayMs int
	}{{"slow", 0}, {"slow", -5}, {"kill", 0}, {"flap", 0}} {
		if _, err := ChaosSteps(bad.action, n, bad.delayMs); err == nil {
			t.Errorf("%s with %d ms accepted", bad.action, bad.delayMs)
		}
	}
}
