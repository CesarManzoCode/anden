package controlplane

import (
	"fmt"
	"net/netip"

	"github.com/CesarManzoCode/anden/internal/store"
)

// resources is CPU and memory reserved or asked for.
type resources struct {
	cpuMillis int64
	memoryMiB int64
}

// firstFit returns the first ready node, in the given order, where the service
// fits on top of what is reserved there, or "" if none does. K4 replaces it
// with the scheduler of D07.
func firstFit(need resources, nodes []store.Node, reserved map[string]resources, ready func(string) bool) string {
	for _, n := range nodes {
		used := reserved[n.Name]
		if ready(n.Name) && used.cpuMillis+need.cpuMillis <= int64(n.CPUMillis) && used.memoryMiB+need.memoryMiB <= int64(n.MemoryMiB) {
			return n.Name
		}
	}
	return ""
}

// nextAddr returns the lowest free address of subnet. The first address is the
// network, the second the gateway of svc0 and the last the broadcast.
func nextAddr(subnet string, used map[string]bool) (string, error) {
	p, err := netip.ParsePrefix(subnet)
	if err != nil {
		return "", fmt.Errorf("subnet of the node: %w", err)
	}
	addr := p.Masked().Addr().Next().Next()
	for next := addr.Next(); p.Contains(next); addr, next = next, next.Next() {
		if !used[addr.String()] {
			return addr.String(), nil
		}
	}
	return "", fmt.Errorf("no free address in %s", subnet)
}
