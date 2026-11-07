// Command placeopt is the external comparison of the scheduler (D07): an
// exhaustive search, independent of the product, over every assignment of
// each service to a node or to no node. That is (k+1)^n leaves for k nodes
// and n services: the 3^n of D07 with full placements, plus the option of
// leaving a service out, which maximizing the services placed needs. Branches
// that can no longer reach the best count are cut, so the answer stays exact.
//
// Usage:
//
//	placeopt --input FILE
//
// FILE is the instance of `anden placement solve`: {"nodes": [{"name",
// "cpu_millis", "mem_mib"}], "services": [{"name", "cpu_millis", "mem_mib",
// "anti_affinity_group"}]}. It prints one JSON object: {"placed",
// "max_occupancy", "placement": {service: node or null}}. The objective is
// to maximize the services placed and, on a tie, to minimize the largest
// occupancy of a node, the larger of its CPU and memory shares. It exits with
// 0, with 1 if the instance cannot be read, and with 2 if the command line is
// wrong.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
)

const (
	exitOK     = 0
	exitFailed = 1
	exitUsage  = 2
	// maxServices is the n of D07 up to which the search is run.
	maxServices = 12
)

type node struct {
	Name      string `json:"name"`
	CPUMillis int64  `json:"cpu_millis"`
	MemMiB    int64  `json:"mem_mib"`
}

type service struct {
	Name              string `json:"name"`
	CPUMillis         int64  `json:"cpu_millis"`
	MemMiB            int64  `json:"mem_mib"`
	AntiAffinityGroup string `json:"anti_affinity_group"`
}

type problem struct {
	Nodes    []node    `json:"nodes"`
	Services []service `json:"services"`
}

type answer struct {
	Placed       int                `json:"placed"`
	MaxOccupancy float64            `json:"max_occupancy"`
	Placement    map[string]*string `json:"placement"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("placeopt", flag.ContinueOnError)
	flags.SetOutput(stderr)
	input := flags.String("input", "", "instance as JSON")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *input == "" {
		fmt.Fprintln(stderr, "usage: placeopt --input FILE")
		return exitUsage
	}
	raw, err := os.ReadFile(*input)
	if err != nil {
		fmt.Fprintln(stderr, "placeopt:", err)
		return exitFailed
	}
	var p problem
	if err := json.Unmarshal(raw, &p); err != nil {
		fmt.Fprintln(stderr, "placeopt: instance:", err)
		return exitFailed
	}
	if len(p.Services) > maxServices {
		fmt.Fprintf(stderr, "placeopt: %d services, more than %d\n", len(p.Services), maxServices)
		return exitFailed
	}
	out, err := json.Marshal(solve(p))
	if err != nil {
		fmt.Fprintln(stderr, "placeopt:", err)
		return exitFailed
	}
	fmt.Fprintln(stdout, string(out))
	return exitOK
}

// search is the state of the exhaustive search. assign[i] is the node index
// of service i, or -1.
type search struct {
	p        problem
	cpu, mem []int64
	groups   []map[string]int
	assign   []int
	best     []int
	bestN    int
	bestOcc  float64
	haveBest bool
}

func solve(p problem) answer {
	s := &search{
		p:      p,
		cpu:    make([]int64, len(p.Nodes)),
		mem:    make([]int64, len(p.Nodes)),
		groups: make([]map[string]int, len(p.Nodes)),
		assign: make([]int, len(p.Services)),
	}
	for k := range s.groups {
		s.groups[k] = map[string]int{}
	}
	s.visit(0, 0)
	a := answer{Placed: s.bestN, MaxOccupancy: s.bestOcc, Placement: map[string]*string{}}
	for i, svc := range p.Services {
		a.Placement[svc.Name] = nil
		if k := s.best[i]; k >= 0 {
			name := p.Nodes[k].Name
			a.Placement[svc.Name] = &name
		}
	}
	return a
}

func share(a, b int64) float64 {
	if b <= 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func (s *search) occupancy() float64 {
	occ := 0.0
	for k, n := range s.p.Nodes {
		occ = max(occ, share(s.cpu[k], n.CPUMillis), share(s.mem[k], n.MemMiB))
	}
	return occ
}

// visit decides service i, with placed services placed among the first i.
func (s *search) visit(i, placed int) {
	if s.haveBest && placed+len(s.p.Services)-i < s.bestN {
		return
	}
	if i == len(s.p.Services) {
		occ := s.occupancy()
		if !s.haveBest || placed > s.bestN || (placed == s.bestN && occ < s.bestOcc) {
			s.haveBest, s.bestN, s.bestOcc = true, placed, occ
			s.best = append(s.best[:0], s.assign...)
		}
		return
	}
	svc := s.p.Services[i]
	for k, n := range s.p.Nodes {
		if s.cpu[k]+svc.CPUMillis > n.CPUMillis || s.mem[k]+svc.MemMiB > n.MemMiB ||
			(svc.AntiAffinityGroup != "" && s.groups[k][svc.AntiAffinityGroup] > 0) {
			continue
		}
		s.cpu[k] += svc.CPUMillis
		s.mem[k] += svc.MemMiB
		s.groups[k][svc.AntiAffinityGroup]++
		s.assign[i] = k
		s.visit(i+1, placed+1)
		s.cpu[k] -= svc.CPUMillis
		s.mem[k] -= svc.MemMiB
		s.groups[k][svc.AntiAffinityGroup]--
	}
	s.assign[i] = -1
	s.visit(i+1, placed)
}
