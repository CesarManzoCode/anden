// Package scheduler is the placement of contract D07: a pure function from an
// instance (nodes and services) to a placement. It filters the nodes with free
// CPU and memory and without a service of the same anti-affinity group, and
// takes the best fit by the dominant resource of the service, processing the
// batch in decreasing order of size. The control plane and
// `anden placement solve` both call Solve.
package scheduler

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Node is what a node offers.
type Node struct {
	Name      string `json:"name"`
	CPUMillis int64  `json:"cpu_millis"`
	MemMiB    int64  `json:"mem_mib"`
}

// Service is what a service asks for. Node, when set, pins it: it already
// runs there, so it reserves that node and is not moved, even if it no longer
// fits. A pinned service on a node outside the instance reserves nothing.
type Service struct {
	Name              string `json:"name"`
	CPUMillis         int64  `json:"cpu_millis"`
	MemMiB            int64  `json:"mem_mib"`
	AntiAffinityGroup string `json:"anti_affinity_group"`
	Node              string `json:"node,omitempty"`
}

// Problem is one instance.
type Problem struct {
	Nodes    []Node    `json:"nodes"`
	Services []Service `json:"services"`
}

// Placement is the node of every service, "" for one that fits nowhere. In
// JSON, that one is null.
type Placement map[string]string

func (p Placement) MarshalJSON() ([]byte, error) {
	out := make(map[string]*string, len(p))
	for svc, n := range p {
		if n == "" {
			out[svc] = nil
		} else {
			out[svc] = &n
		}
	}
	return json.Marshal(out)
}

// Check rejects an instance Solve cannot answer: names empty or repeated, or
// negative amounts.
func Check(p Problem) error {
	nodes := map[string]bool{}
	for _, n := range p.Nodes {
		if n.Name == "" || nodes[n.Name] {
			return fmt.Errorf("node name %q is empty or repeated", n.Name)
		}
		if n.CPUMillis < 0 || n.MemMiB < 0 {
			return fmt.Errorf("node %s: negative capacity", n.Name)
		}
		nodes[n.Name] = true
	}
	services := map[string]bool{}
	for _, s := range p.Services {
		if s.Name == "" || services[s.Name] {
			return fmt.Errorf("service name %q is empty or repeated", s.Name)
		}
		if s.CPUMillis < 0 || s.MemMiB < 0 {
			return fmt.Errorf("service %s: negative request", s.Name)
		}
		services[s.Name] = true
	}
	return nil
}

// share is a/b, or 0 when there is nothing to share.
func share(a, b int64) float64 {
	if b <= 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// node is a node with what is already reserved on it.
type node struct {
	Node
	cpu, mem int64
	groups   map[string]bool
}

func (n *node) fits(s Service) bool {
	return n.cpu+s.CPUMillis <= n.CPUMillis && n.mem+s.MemMiB <= n.MemMiB &&
		(s.AntiAffinityGroup == "" || !n.groups[s.AntiAffinityGroup])
}

func (n *node) reserve(s Service) {
	n.cpu += s.CPUMillis
	n.mem += s.MemMiB
	if s.AntiAffinityGroup != "" {
		n.groups[s.AntiAffinityGroup] = true
	}
}

// Solve places every service of p. Pinned services stay where they are;
// the rest go, largest first, each to the feasible node that is left with
// the least free share of the service's dominant resource. Ties go to the
// smaller name, for services, and to the earlier node of p.Nodes. The same
// instance always gives the same placement.
func Solve(p Problem) Placement {
	nodes := make([]*node, len(p.Nodes))
	byName := map[string]*node{}
	var totalCPU, totalMem int64
	for i, n := range p.Nodes {
		nodes[i] = &node{Node: n, groups: map[string]bool{}}
		byName[n.Name] = nodes[i]
		totalCPU += n.CPUMillis
		totalMem += n.MemMiB
	}
	out := Placement{}
	var batch []Service
	for _, s := range p.Services {
		if s.Node == "" {
			batch = append(batch, s)
			continue
		}
		if n, ok := byName[s.Node]; ok {
			n.reserve(s)
			out[s.Name] = s.Node
		} else {
			out[s.Name] = ""
		}
	}

	// The size of a service is its dominant share of the whole cluster.
	cpuDominant := func(s Service) bool {
		return share(s.CPUMillis, totalCPU) >= share(s.MemMiB, totalMem)
	}
	size := func(s Service) float64 {
		return max(share(s.CPUMillis, totalCPU), share(s.MemMiB, totalMem))
	}
	sort.SliceStable(batch, func(i, j int) bool {
		si, sj := size(batch[i]), size(batch[j])
		if si != sj {
			return si > sj
		}
		return batch[i].Name < batch[j].Name
	})

	for _, s := range batch {
		var best *node
		bestLeft := 0.0
		for _, n := range nodes {
			if !n.fits(s) {
				continue
			}
			var left float64
			if cpuDominant(s) {
				left = share(n.CPUMillis-n.cpu-s.CPUMillis, n.CPUMillis)
			} else {
				left = share(n.MemMiB-n.mem-s.MemMiB, n.MemMiB)
			}
			if best == nil || left < bestLeft {
				best, bestLeft = n, left
			}
		}
		if best == nil {
			out[s.Name] = ""
			continue
		}
		best.reserve(s)
		out[s.Name] = best.Name
	}
	return out
}
