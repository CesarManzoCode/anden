package scheduler

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

func threeNodes(cpu, mem int64) []Node {
	return []Node{{"node1", cpu, mem}, {"node2", cpu, mem}, {"node3", cpu, mem}}
}

func svc(name string, cpu, mem int64, group string) Service {
	return Service{Name: name, CPUMillis: cpu, MemMiB: mem, AntiAffinityGroup: group}
}

func TestCapacity(t *testing.T) {
	got := Solve(Problem{
		Nodes:    threeNodes(1000, 1000),
		Services: []Service{svc("a", 600, 100, ""), svc("b", 600, 100, ""), svc("c", 400, 100, ""), svc("d", 1000, 1000, "")},
	})
	// Largest first: d fills node1, a and b split, and c fits exactly next to a.
	want := Placement{"d": "node1", "a": "node2", "b": "node3", "c": "node2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	used := map[string][2]int64{}
	for _, s := range []Service{svc("a", 600, 100, ""), svc("b", 600, 100, ""), svc("c", 400, 100, ""), svc("d", 1000, 1000, "")} {
		u := used[got[s.Name]]
		used[got[s.Name]] = [2]int64{u[0] + s.CPUMillis, u[1] + s.MemMiB}
	}
	for n, u := range used {
		if u[0] > 1000 || u[1] > 1000 {
			t.Errorf("%s over capacity: %v", n, u)
		}
	}
}

func TestBestFitByDominantResource(t *testing.T) {
	// m is memory dominant: it goes where the least memory is left after it,
	// although node1 has the least CPU left.
	got := Solve(Problem{
		Nodes: threeNodes(1000, 1000),
		Services: []Service{
			{Name: "cpu", CPUMillis: 800, MemMiB: 100, Node: "node1"},
			{Name: "mem", CPUMillis: 100, MemMiB: 700, Node: "node2"},
			svc("m", 100, 300, ""),
		},
	})
	if got["m"] != "node2" {
		t.Fatalf("m on %q, want node2: %v", got["m"], got)
	}
}

func TestAntiAffinity(t *testing.T) {
	services := []Service{svc("a1", 100, 100, "g"), svc("a2", 100, 100, "g"), svc("a3", 100, 100, "g"), svc("a4", 100, 100, "g"), svc("b", 100, 100, "")}
	got := Solve(Problem{Nodes: threeNodes(1000, 1000), Services: services})
	seen := map[string]bool{}
	for _, name := range []string{"a1", "a2", "a3"} {
		if got[name] == "" || seen[got[name]] {
			t.Fatalf("%s on %q shares a node with its group: %v", name, got[name], got)
		}
		seen[got[name]] = true
	}
	if got["a4"] != "" {
		t.Errorf("a fourth member of g has no node left: %v", got)
	}
	if got["b"] == "" {
		t.Errorf("b has no group and fits: %v", got)
	}
	// A pinned member counts too.
	got = Solve(Problem{Nodes: threeNodes(1000, 1000)[:1], Services: []Service{
		{Name: "p", CPUMillis: 1, MemMiB: 1, AntiAffinityGroup: "g", Node: "node1"}, svc("q", 1, 1, "g"),
	}})
	if got["p"] != "node1" || got["q"] != "" {
		t.Errorf("pinned group member: %v", got)
	}
}

func TestDoesNotFitIsNull(t *testing.T) {
	got := Solve(Problem{
		Nodes:    threeNodes(1000, 1000),
		Services: []Service{svc("big", 1001, 1, ""), svc("fat", 1, 1001, ""), svc("ok", 1000, 1000, "")},
	})
	if got["big"] != "" || got["fat"] != "" || got["ok"] != "node1" {
		t.Fatalf("%v", got)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]*string
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if len(back) != 3 || back["big"] != nil || back["fat"] != nil || back["ok"] == nil || *back["ok"] != "node1" {
		t.Errorf("JSON: %s", raw)
	}
	if string(raw) != `{"big":null,"fat":null,"ok":"node1"}` {
		t.Errorf("JSON: %s", raw)
	}
}

func TestPinnedStaysAndReserves(t *testing.T) {
	got := Solve(Problem{Nodes: threeNodes(1000, 1000)[:2], Services: []Service{
		{Name: "old", CPUMillis: 900, MemMiB: 100, Node: "node2"},
		{Name: "gone", CPUMillis: 900, MemMiB: 100, Node: "node9"},
		svc("new", 500, 100, ""),
	}})
	if got["old"] != "node2" || got["gone"] != "" || got["new"] != "node1" {
		t.Fatalf("%v", got)
	}
}

func TestDeterministic(t *testing.T) {
	p := Problem{Nodes: threeNodes(2000, 2048)}
	for i := range 12 {
		group := ""
		if i%3 == 0 {
			group = "g"
		}
		// Many equal sizes, so that every tie is decided by the rules.
		p.Services = append(p.Services, svc(fmt.Sprintf("s%02d", 11-i), int64(300+100*(i%4)), 256, group))
	}
	first := Solve(p)
	firstJSON, _ := json.Marshal(first)
	for range 50 {
		again := Solve(p)
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("got %v, then %v", first, again)
		}
		raw, _ := json.Marshal(again)
		if string(raw) != string(firstJSON) {
			t.Fatalf("JSON %s, then %s", firstJSON, raw)
		}
	}
	// The order of the input does not change the answer.
	rev := Problem{Nodes: p.Nodes}
	for i := len(p.Services) - 1; i >= 0; i-- {
		rev.Services = append(rev.Services, p.Services[i])
	}
	if got := Solve(rev); !reflect.DeepEqual(first, got) {
		t.Errorf("reversed input: %v, want %v", got, first)
	}
}

func TestCheck(t *testing.T) {
	for _, p := range []Problem{
		{Nodes: []Node{{"n", 1, 1}, {"n", 1, 1}}},
		{Nodes: []Node{{"", 1, 1}}},
		{Nodes: []Node{{"n", -1, 1}}},
		{Services: []Service{svc("a", 1, 1, ""), svc("a", 1, 1, "")}},
		{Services: []Service{svc("a", 1, -1, "")}},
	} {
		if Check(p) == nil {
			t.Errorf("accepted %+v", p)
		}
	}
	if err := Check(Problem{Nodes: threeNodes(1, 1), Services: []Service{svc("a", 1, 1, "")}}); err != nil {
		t.Error(err)
	}
}
