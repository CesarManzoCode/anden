package controlplane

import (
	"bytes"
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"strings"
)

// edgeTarget is where an edge port leads: the address of a service and its port.
type edgeTarget struct {
	addr string
	port uint32
}

// Edge keeps table ip anden_edge (D21) equal to the wanted map, changing only
// the elements that differ, always in one nft transaction.
type Edge struct {
	ip    string
	apply func(script string) error
	// applied is nil until the table exists: a new andend replaces the table
	// a previous one left, in the same transaction that fills it.
	applied map[int32]edgeTarget
}

func NewEdge(ip string) *Edge {
	return &Edge{ip: ip, apply: runNft}
}

// Sync programs the differences between what is applied and want.
func (e *Edge) Sync(want map[int32]edgeTarget) error {
	if want == nil {
		want = map[int32]edgeTarget{}
	}
	script := edgeScript(e.ip, e.applied, want)
	if script == "" {
		return nil
	}
	if err := e.apply(script); err != nil {
		return err
	}
	e.applied = want
	return nil
}

func element(port int32, t edgeTarget) string {
	return fmt.Sprintf("add element ip anden_edge svc { %d : %s . %d }\n", port, t.addr, t.port)
}

// edgeScript is the nft -f - input that goes from applied to want, or "" when
// they are equal. The delete comes first because add does not replace the value
// of a key.
func edgeScript(ip string, applied, want map[int32]edgeTarget) string {
	var b strings.Builder
	if applied == nil {
		// The empty table makes the delete valid when none exists yet.
		fmt.Fprintf(&b, `table ip anden_edge {}
delete table ip anden_edge
table ip anden_edge {
  map svc { type inet_service : ipv4_addr . inet_service; }
  chain prerouting { type nat hook prerouting priority dstnat; ip daddr %[1]s dnat ip to tcp dport map @svc; }
  chain output { type nat hook output priority dstnat; ip daddr %[1]s dnat ip to tcp dport map @svc; }
}
`, ip)
	}
	for _, port := range slices.Sorted(maps.Keys(applied)) {
		if t, ok := want[port]; !ok || t != applied[port] {
			fmt.Fprintf(&b, "delete element ip anden_edge svc { %d }\n", port)
		}
	}
	for _, port := range slices.Sorted(maps.Keys(want)) {
		if old, ok := applied[port]; !ok || old != want[port] {
			b.WriteString(element(port, want[port]))
		}
	}
	return b.String()
}

func runNft(script string) error {
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("nft -f -: %w: %s", err, out.String())
	}
	return nil
}
