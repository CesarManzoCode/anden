package agent

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
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

// instanceBatch is the `ip -batch -` input in the instance's netns.
func instanceBatch(peer, addr string, bits int, gateway string) []string {
	return []string{
		"link set lo up",
		"link set " + peer + " name eth0",
		fmt.Sprintf("addr add %s/%d dev eth0", addr, bits),
		"link set eth0 up",
		"route add default via " + gateway,
	}
}
