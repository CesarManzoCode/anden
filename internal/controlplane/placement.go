package controlplane

import (
	"fmt"
	"net/netip"
)

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
