package controlplane

import (
	"net/netip"
	"testing"
)

// servicesPerNode is the top step of C26 (D12), and best-fit may put every
// sleeping service on one node, so each subnet must hold that many alone.
const servicesPerNode = 1000

// Each subnet of section 3 gives servicesPerNode addresses, all distinct across
// the three nodes, inside their subnet and never its network, gateway or
// broadcast.
func TestNextAddrHoldsAThousandServicesPerNode(t *testing.T) {
	used := map[string]bool{}
	for _, subnet := range []string{"10.81.0.0/16", "10.82.0.0/16", "10.83.0.0/16"} {
		p := netip.MustParsePrefix(subnet)
		network := p.Masked().Addr()
		reserved := map[netip.Addr]bool{network: true, network.Next(): true}
		for i := 0; i < servicesPerNode; i++ {
			got, err := nextAddr(subnet, used)
			if err != nil {
				t.Fatalf("%s, service %d: %v", subnet, i, err)
			}
			addr, err := netip.ParseAddr(got)
			if err != nil {
				t.Fatal(err)
			}
			if used[got] {
				t.Fatalf("%s given twice", got)
			}
			if !p.Contains(addr) || reserved[addr] {
				t.Fatalf("%s, service %d: %s is not a service address", subnet, i, got)
			}
			used[got] = true
		}
	}
	if len(used) != 3*servicesPerNode {
		t.Fatalf("%d addresses, want %d", len(used), 3*servicesPerNode)
	}
}

// Freed addresses are given again, lowest first, without touching the ones in use.
func TestNextAddrReusesFreedAddresses(t *testing.T) {
	const subnet = "10.81.0.0/16"
	used := map[string]bool{}
	var order []string
	for i := 0; i < servicesPerNode; i++ {
		got, err := nextAddr(subnet, used)
		if err != nil {
			t.Fatal(err)
		}
		used[got] = true
		order = append(order, got)
	}
	delete(used, order[700])
	delete(used, order[10])
	for _, want := range []string{order[10], order[700]} {
		got, err := nextAddr(subnet, used)
		if err != nil || got != want {
			t.Fatalf("nextAddr = %q, %v; want %q", got, err, want)
		}
		used[got] = true
	}
	if got, err := nextAddr(subnet, used); err != nil || used[got] {
		t.Fatalf("after reuse: %q, %v", got, err)
	}
}
