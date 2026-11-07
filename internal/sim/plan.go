// Package sim is Andén's datacenter simulator (contract, section 3): a
// delegated systemd user scope with Postgres and one user+pid+net+mount
// namespace that holds the control plane, three nodes and an external
// client. This file has the pure parts: names, paths, the address plan and
// the network commands.
package sim

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	NodeCount  = 3
	Bridge     = "anden0"
	RootIP     = "10.80.0.1"
	EdgeIP     = "10.80.0.2"
	ClientIP   = "10.80.0.100"
	AgentPort  = 7443
	BlobsPort  = 7444
	DNSPort    = 53
	ClientName = "client"
	RootName   = "root"

	// Controllers enabled in every cgroup that has children.
	Controllers = "+cpu +memory +pids"
	dcFile      = "dc.json"
	logFile     = "dc.log"
)

// Node is one simulated node of the address plan.
type Node struct {
	Name     string // node1
	IP       string // 10.80.0.11, on anden0
	Subnet   string // 10.81.0.0/16, services on the node's svc0
	Gateway  string // 10.81.0.1, address of svc0
	RootVeth string // veth-node1, root side of the node's link
}

// Nodes returns the address plan of section 3.
func Nodes() []Node {
	nodes := make([]Node, 0, NodeCount)
	for i := 1; i <= NodeCount; i++ {
		name := "node" + strconv.Itoa(i)
		nodes = append(nodes, Node{
			Name:     name,
			IP:       fmt.Sprintf("10.80.0.%d", 10+i),
			Subnet:   fmt.Sprintf("10.%d.0.0/16", 80+i),
			Gateway:  fmt.Sprintf("10.%d.0.1", 80+i),
			RootVeth: "veth-" + name,
		})
	}
	return nodes
}

// FindNode returns the node with that name.
func FindNode(name string) (Node, bool) {
	for _, n := range Nodes() {
		if n.Name == name {
			return n, true
		}
	}
	return Node{}, false
}

// Hash is h of section 3: the first 12 hex digits of the sha1 of the
// absolute state path.
func Hash(absState string) string {
	sum := sha1.Sum([]byte(absState))
	return hex.EncodeToString(sum[:])[:12]
}

// StatePath returns the absolute state path and its h.
func StatePath(state string) (abs, h string, err error) {
	abs, err = filepath.Abs(state)
	if err != nil {
		return "", "", err
	}
	return abs, Hash(abs), nil
}

// Unit is the name of the systemd scope, without the .scope suffix.
func Unit(h string) string { return "anden-" + h }

// SocketDir is where the Unix sockets live: under XDG_RUNTIME_DIR, or /tmp
// when it is not set, because a state path can exceed sun_path.
func SocketDir(h, runtimeDir string) string {
	if runtimeDir == "" {
		runtimeDir = "/tmp"
	}
	return filepath.Join(runtimeDir, "anden-"+h)
}

// APISocket is the path of andend's socket for host clients.
func APISocket(sockets string) string { return filepath.Join(sockets, "api.sock") }

// DC is the content of STATE/dc.json. Pids are in the pid namespace of the
// host, so that nsenter works from outside.
type DC struct {
	ScopeCgroup string         `json:"scope_cgroup"`
	DCPid       int            `json:"dc_pid"`
	NS          map[string]int `json:"ns"`
	PGSocketDir string         `json:"pg_socket_dir"`
	APISocket   string         `json:"api_socket"`
	EdgeIP      string         `json:"edge_ip"`
}

// ReadDC reads STATE/dc.json.
func ReadDC(state string) (*DC, error) {
	raw, err := os.ReadFile(filepath.Join(state, dcFile))
	if err != nil {
		return nil, err
	}
	var dc DC
	if err := json.Unmarshal(raw, &dc); err != nil {
		return nil, fmt.Errorf("%s: %w", dcFile, err)
	}
	return &dc, nil
}

// peerVeth is the temporary root-side name of the link end that moves to
// the peer's netns, where it becomes eth0.
func peerVeth(name string) string { return "vp-" + name }

func rootVeth(name string) string { return "veth-" + name }

// RootLinkBatch is the `ip -batch -` input of the root netns: the bridge
// with the control plane and edge addresses, and one veth per peer whose
// other end moves to the netns held by pids[peer]. Peers are the client
// and the nodes.
func RootLinkBatch(pids map[string]int) []string {
	lines := []string{
		"link set lo up",
		"link add " + Bridge + " type bridge",
		"addr add " + RootIP + "/24 dev " + Bridge,
		"addr add " + EdgeIP + "/32 dev " + Bridge,
		"link set " + Bridge + " up",
	}
	for _, peer := range peers() {
		lines = append(lines,
			"link add "+rootVeth(peer)+" type veth peer name "+peerVeth(peer),
			"link set "+peerVeth(peer)+" netns "+strconv.Itoa(pids[peer]),
			"link set "+rootVeth(peer)+" master "+Bridge,
			"link set "+rootVeth(peer)+" up",
		)
	}
	return lines
}

// RootRouteBatch routes each node's service subnet through the node. It
// runs once the node ends are up.
func RootRouteBatch() []string {
	var lines []string
	for _, n := range Nodes() {
		lines = append(lines, "route add "+n.Subnet+" via "+n.IP)
	}
	return lines
}

// PeerBatch is the `ip -batch -` input inside a peer's netns: eth0 with its
// address and the default route to the root netns and, for a node, the
// svc0 bridge of its services.
func PeerBatch(peer string) ([]string, error) {
	ip := ClientIP
	node, isNode := FindNode(peer)
	if isNode {
		ip = node.IP
	} else if peer != ClientName {
		return nil, fmt.Errorf("unknown peer %q", peer)
	}
	lines := []string{
		"link set lo up",
		"link set " + peerVeth(peer) + " name eth0",
		"addr add " + ip + "/24 dev eth0",
		"link set eth0 up",
		"route add default via " + RootIP,
	}
	if isNode {
		lines = append(lines,
			"link add svc0 type bridge",
			"addr add "+node.Gateway+"/16 dev svc0",
			"link set svc0 up",
			// The client shares eth0's subnet, but a reply must go back through
			// the root netns, or the DNAT of the edge is never undone.
			"route add "+ClientIP+"/32 via "+RootIP,
		)
	}
	return lines, nil
}

// peers are the netns of the datacenter other than the root one.
func peers() []string {
	names := []string{ClientName}
	for _, n := range Nodes() {
		names = append(names, n.Name)
	}
	return names
}

// HolderArgv0 names the process that holds a peer's netns, so that lsns
// shows which datacenter namespace it is.
func HolderArgv0(peer string) string { return "anden-hold-" + peer }

// NodeDisk is the node's disk directory, bind mounted on itself in the
// node's mount namespace.
func NodeDisk(state, node string) string { return filepath.Join(state, "nodes", node) }

// AgentArgs is the command line of a node's agent, after the binary.
func AgentArgs(n Node, state, dcCgroup, binDir string) []string {
	return []string{
		"--node", n.Name,
		"--addr", n.IP,
		"--control", RootIP + ":" + strconv.Itoa(AgentPort),
		"--state", NodeDisk(state, n.Name),
		"--cgroup", filepath.Join(dcCgroup, n.Name),
		"--subnet", n.Subnet,
		"--runtime", filepath.Join(binDir, "anden-runtime"),
	}
}

// AndendArgs is the command line of the control plane, after the binary.
func AndendArgs(state, sockets string) []string {
	return []string{
		"--api-socket", APISocket(sockets),
		"--listen", RootIP + ":" + strconv.Itoa(AgentPort),
		"--pg-socket-dir", sockets,
		"--registry", filepath.Join(state, "registry"),
		"--blobs-listen", RootIP + ":" + strconv.Itoa(BlobsPort),
		"--edge-ip", EdgeIP,
		"--dns-listen", RootIP + ":" + strconv.Itoa(DNSPort),
	}
}

// NodeCapacity is what one node offers: an equal share of the machine.
func NodeCapacity(cpus int, memTotalKiB int64) (cpuMillis, memoryMiB int64) {
	return int64(cpus) * 1000 / NodeCount, memTotalKiB / 1024 / NodeCount
}

// parseMemTotal returns MemTotal of /proc/meminfo, in KiB.
func parseMemTotal(meminfo []byte) (int64, error) {
	for _, line := range strings.Split(string(meminfo), "\n") {
		rest, ok := strings.CutPrefix(line, "MemTotal:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			break
		}
		return strconv.ParseInt(fields[0], 10, 64)
	}
	return 0, fmt.Errorf("no MemTotal in meminfo")
}

// ChaosStep is one command of a network fault of D06, run in the root netns.
// An optional one may fail: heal removes a delay that may not be there.
type ChaosStep struct {
	Cmd      []string
	Optional bool
}

// ChaosSteps are the commands of a network fault on the root side of the
// node's link: partition takes veth-nodeN down, heal brings it up and removes
// any delay, and slow adds delayMs to what goes to the node, with netem.
func ChaosSteps(action string, n Node, delayMs int) ([]ChaosStep, error) {
	switch action {
	case "partition":
		return []ChaosStep{{Cmd: []string{"ip", "link", "set", n.RootVeth, "down"}}}, nil
	case "heal":
		return []ChaosStep{
			{Cmd: []string{"tc", "qdisc", "del", "dev", n.RootVeth, "root"}, Optional: true},
			{Cmd: []string{"ip", "link", "set", n.RootVeth, "up"}},
		}, nil
	case "slow":
		if delayMs <= 0 {
			return nil, fmt.Errorf("slow needs --delay-ms above 0")
		}
		return []ChaosStep{{Cmd: []string{"tc", "qdisc", "replace", "dev", n.RootVeth, "root", "netem", "delay",
			strconv.Itoa(delayMs) + "ms"}}}, nil
	}
	return nil, fmt.Errorf("unknown chaos action %q", action)
}

// ExecArgs is the nsenter command line of `anden exec`.
func ExecArgs(pid int, cmd []string) []string {
	return append([]string{"--preserve-credentials", "-U", "-n", "-t", strconv.Itoa(pid), "--"}, cmd...)
}

// cgroupDir turns the 0:: line of /proc/<pid>/cgroup into a directory
// under /sys/fs/cgroup.
func cgroupDir(procCgroup []byte) (string, error) {
	for _, line := range strings.Split(string(procCgroup), "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			return filepath.Join("/sys/fs/cgroup", rest), nil
		}
	}
	return "", fmt.Errorf("no cgroup v2 entry in /proc/self/cgroup")
}

// parseNSpid returns the NSpid field of /proc/<pid>/status, outermost
// namespace first.
func parseNSpid(status []byte) ([]int, error) {
	for _, line := range strings.Split(string(status), "\n") {
		rest, ok := strings.CutPrefix(line, "NSpid:")
		if !ok {
			continue
		}
		var pids []int
		for _, f := range strings.Fields(rest) {
			p, err := strconv.Atoi(f)
			if err != nil {
				return nil, fmt.Errorf("NSpid %q: %w", rest, err)
			}
			pids = append(pids, p)
		}
		if len(pids) == 0 {
			break
		}
		return pids, nil
	}
	return nil, fmt.Errorf("no NSpid in status")
}
