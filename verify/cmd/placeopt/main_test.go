package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSolveMaximizesPlacedThenBalances(t *testing.T) {
	// Two of the three 600s cannot share a node; the best keeps all four
	// placed and puts the small one beside a 600, not alone.
	p := problem{
		Nodes: []node{{"n1", 1000, 1000}, {"n2", 1000, 1000}, {"n3", 1000, 1000}},
		Services: []service{
			{"a", 600, 100, ""}, {"b", 600, 100, ""}, {"c", 600, 100, ""}, {"d", 400, 100, ""},
		},
	}
	got := solve(p)
	if got.Placed != 4 || got.MaxOccupancy != 1.0 {
		t.Fatalf("%+v", got)
	}
	// Without d, the three 600s spread with occupancy 0.6.
	p.Services = p.Services[:3]
	if got := solve(p); got.Placed != 3 || got.MaxOccupancy != 0.6 {
		t.Fatalf("%+v", got)
	}
}

func TestSolveAntiAffinityAndNull(t *testing.T) {
	p := problem{
		Nodes:    []node{{"n1", 1000, 1000}, {"n2", 1000, 1000}},
		Services: []service{{"a", 1, 1, "g"}, {"b", 1, 1, "g"}, {"c", 1, 1, "g"}, {"big", 2000, 1, ""}},
	}
	got := solve(p)
	if got.Placed != 2 {
		t.Fatalf("%+v", got)
	}
	nulls := 0
	for _, n := range got.Placement {
		if n == nil {
			nulls++
		}
	}
	if nulls != 2 || got.Placement["big"] != nil {
		t.Errorf("%+v", got.Placement)
	}
}

func TestRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.json")
	if err := os.WriteFile(path, []byte(`{"nodes":[{"name":"n1","cpu_millis":10,"mem_mib":10}],"services":[{"name":"a","cpu_millis":5,"mem_mib":5,"anti_affinity_group":""},{"name":"b","cpu_millis":6,"mem_mib":1,"anti_affinity_group":""}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if rc := run([]string{"--input", path}, &out, &errOut); rc != exitOK {
		t.Fatalf("rc %d: %s", rc, errOut.String())
	}
	var a answer
	if err := json.Unmarshal(out.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	if a.Placed != 1 || len(a.Placement) != 2 {
		t.Errorf("%s", out.String())
	}
	if rc := run(nil, &out, &errOut); rc != exitUsage {
		t.Errorf("no input: rc %d", rc)
	}
}
