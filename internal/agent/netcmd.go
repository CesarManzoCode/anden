package agent

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/netip"
)

const svcBridge = "svc0"

// vethNames are the two ends of the veth of an instance, derived from its id
// because an interface name has at most 15 bytes.
func vethNames(id string) (host, peer string) {
	sum := sha1.Sum([]byte(id))
	h := hex.EncodeToString(sum[:])[:10]
	return "vh" + h, "vc" + h
}

// namespaceArgs is the unshare command line that creates the persistent
// network, UTS and IPC namespaces of an instance. The files must exist.
func namespaceArgs(netNS, utsNS, ipcNS string) []string {
	return []string{"--net=" + netNS, "--uts=" + utsNS, "--ipc=" + ipcNS, "true"}
}

// hostBatch is the `ip -batch -` input in the node's netns: the veth with its
// peer in the instance's netns and its host end on svc0.
func hostBatch(host, peer, netNS string) []string {
	return []string{
		"link add " + host + " type veth peer name " + peer,
		"link set " + peer + " netns " + netNS,
		"link set " + host + " master " + svcBridge,
		"link set " + host + " up",
	}
}

// macOf derives the MAC of an instance from its address. It stays the same across
// a sleep, because the node and its neighbors keep it in their ARP caches: with a
// new one, their first packets to the woken instance would go nowhere for seconds.
func macOf(addr string) (string, error) {
	a, err := netip.ParseAddr(addr)
	if err != nil {
		return "", err
	}
	if !a.Is4() {
		return "", fmt.Errorf("%s is not an IPv4 address", addr)
	}
	b := a.As4()
	return fmt.Sprintf("02:00:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3]), nil
}

// instanceBatch is the `ip -batch -` input in the instance's netns. eth0 has no
// IPv6 link-local address: it would send router solicitations for no one, and
// the idle signal counts what eth0 sends as what the service does.
func instanceBatch(peer, mac, addr string, bits int, gateway string) []string {
	return []string{
		"link set lo up",
		"link set " + peer + " name eth0",
		"link set eth0 address " + mac,
		"link set eth0 addrgenmode none",
		fmt.Sprintf("addr add %s/%d dev eth0", addr, bits),
		"link set eth0 up",
		"route add default via " + gateway,
	}
}
