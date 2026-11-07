package controlplane

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	andenv1 "github.com/CesarManzoCode/anden/gen/anden/v1"
	"github.com/CesarManzoCode/anden/internal/store"
)

// memState is an in-memory State with the same revision rules as the store.
type memState struct {
	services  map[string]store.Service
	instances []store.Instance
	nodes     []store.Node
}

func (m *memState) ListServices(_ context.Context, includeDeleted bool) ([]store.Service, error) {
	var out []store.Service
	for _, s := range m.services {
		if includeDeleted || !s.Deleted {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b store.Service) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (m *memState) ListNodes(context.Context) ([]store.Node, error) { return m.nodes, nil }

func (m *memState) SetPlacement(_ context.Context, name string, revision int64, node, addr string) (bool, error) {
	s, ok := m.services[name]
	if !ok || s.Revision != revision {
		return false, nil
	}
	s.DesiredNode, s.DesiredAddr = node, addr
	m.services[name] = s
	return true, nil
}

func (m *memState) Instances(context.Context) ([]store.Instance, error) { return m.instances, nil }

func (m *memState) SetObserved(_ context.Context, name string, revision int64, obs store.Observed) (bool, error) {
	s, ok := m.services[name]
	if !ok || s.Revision != revision {
		return false, nil
	}
	s.State, s.Node, s.Addr = obs.State, obs.Node, obs.Addr
	m.services[name] = s
	return true, nil
}

func (m *memState) Purge(_ context.Context, name string, revision int64) error {
	if s, ok := m.services[name]; ok && s.Deleted && s.Revision == revision {
		delete(m.services, name)
	}
	return nil
}

func running(service string, version int64, node, addr string) store.Instance {
	return store.Instance{ID: service + "-1", Service: service, Node: node, Version: version, State: instRunning, Addr: addr}
}

func TestDecide(t *testing.T) {
	cases := []struct {
		name      string
		svc       store.Service
		instances []store.Instance
		want      string
		purge     bool
	}{
		{"created without agents stays creating", store.Service{DesiredAwake: true, State: stateCreating}, nil, stateCreating, false},
		{"running instance makes it active", store.Service{DesiredAwake: true, State: stateCreating, Version: 1},
			[]store.Instance{running("s", 1, "node1", "10.81.0.2")}, stateActive, false},
		{"sleep with instance still running", store.Service{DesiredAwake: false, State: stateActive},
			[]store.Instance{running("s", 1, "node1", "10.81.0.2")}, stateSleeping, false},
		{"sleep without instance is asleep", store.Service{DesiredAwake: false, State: stateSleeping}, nil, stateAsleep, false},
		{"wake without agents is waking", store.Service{DesiredAwake: true, State: stateAsleep}, nil, stateWaking, false},
		{"failed instance of the desired version", store.Service{DesiredAwake: true, State: stateWaking, Version: 2},
			[]store.Instance{{Service: "s", Version: 2, State: instFailed}}, stateFailed, false},
		{"failed old version does not fail the service", store.Service{DesiredAwake: true, State: stateWaking, Version: 2},
			[]store.Instance{{Service: "s", Version: 1, State: instFailed}}, stateWaking, false},
		{"failed old generation does not fail the service", store.Service{DesiredAwake: true, State: stateWaking, Version: 1, Generation: 2},
			[]store.Instance{{Service: "s", Version: 1, Generation: 1, State: instFailed}}, stateWaking, false},
		{"deleted with nothing left is purged", store.Service{Deleted: true, State: stateActive}, nil, stateActive, true},
		{"deleted with a live instance waits", store.Service{Deleted: true, State: stateActive},
			[]store.Instance{{Service: "s", State: instStarting}}, stateActive, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obs, purge := Decide(c.svc, c.instances)
			if obs.State != c.want || purge != c.purge {
				t.Fatalf("got %s purge=%v, want %s purge=%v", obs.State, purge, c.want, c.purge)
			}
		})
	}
}

func TestDecideTakesAddressOfNewestRunning(t *testing.T) {
	svc := store.Service{DesiredAwake: true, State: stateActive, Version: 2}
	obs, _ := Decide(svc, []store.Instance{running("s", 1, "node1", "10.81.0.2"), running("s", 2, "node2", "10.82.0.3")})
	if obs.Node != "node2" || obs.Addr != "10.82.0.3" {
		t.Fatalf("got %+v", obs)
	}
}

func TestPassPersistsAndPurges(t *testing.T) {
	m := &memState{
		services: map[string]store.Service{
			"a": {Name: "a", Revision: 1, State: stateSleeping},
			"b": {Name: "b", Revision: 3, DesiredAwake: true, State: stateCreating, Version: 1},
			"c": {Name: "c", Revision: 2, Deleted: true, State: stateAsleep},
		},
		instances: []store.Instance{running("b", 1, "node3", "10.83.0.2")},
	}
	r := NewReconciler(m)
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.services["a"].State; got != stateAsleep {
		t.Errorf("a: %s, want %s", got, stateAsleep)
	}
	if got := m.services["b"]; got.State != stateActive || got.Node != "node3" || got.Addr != "10.83.0.2" {
		t.Errorf("b: %+v", got)
	}
	if _, ok := m.services["c"]; ok {
		t.Errorf("c was not purged")
	}
	// A second pass over a converged state changes nothing.
	before := m.services["b"]
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if after := m.services["b"]; after.State != before.State || after.Node != before.Node || after.Addr != before.Addr {
		t.Errorf("second pass changed b: %+v", m.services["b"])
	}
}

// staleState bumps the revision between the read and the write, as an API
// call landing in the middle of a pass would.
type staleState struct{ *memState }

func (s staleState) SetObserved(ctx context.Context, name string, revision int64, obs store.Observed) (bool, error) {
	svc := s.services[name]
	svc.Revision++
	svc.DesiredAwake = true
	svc.State = stateWaking
	s.services[name] = svc
	return s.memState.SetObserved(ctx, name, revision, obs)
}

func TestPassLosesAgainstNewerRevision(t *testing.T) {
	m := &memState{services: map[string]store.Service{"a": {Name: "a", Revision: 1, State: stateSleeping}}}
	if err := NewReconciler(staleState{m}).Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.services["a"].State; got != stateWaking {
		t.Fatalf("stale observed state overwrote the API change: %s", got)
	}
}

func TestKickDoesNotBlock(t *testing.T) {
	r := NewReconciler(&memState{})
	r.Kick()
	r.Kick()
	if len(r.kick) != 1 {
		t.Fatalf("pending kicks: %d", len(r.kick))
	}
}

func specBytes(t *testing.T, cpu, mem uint32, port uint32) []byte {
	t.Helper()
	raw, err := proto.Marshal(&andenv1.ServiceSpec{CpuMillis: cpu, MemoryMib: mem, Port: port})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func twoNodes() []store.Node {
	return []store.Node{
		{Name: "node1", Subnet: "10.81.0.0/16", CPUMillis: 1000, MemoryMiB: 1000},
		{Name: "node2", Subnet: "10.82.0.0/16", CPUMillis: 1000, MemoryMiB: 1000},
	}
}

func TestFirstFit(t *testing.T) {
	ready := func(string) bool { return true }
	nodes := twoNodes()
	if got := firstFit(resources{500, 500}, nodes, nil, ready); got != "node1" {
		t.Errorf("empty nodes: %q", got)
	}
	if got := firstFit(resources{500, 500}, nodes, map[string]resources{"node1": {600, 0}}, ready); got != "node2" {
		t.Errorf("no CPU left on node1: %q", got)
	}
	if got := firstFit(resources{500, 500}, nodes, map[string]resources{"node1": {0, 600}}, ready); got != "node2" {
		t.Errorf("no memory left on node1: %q", got)
	}
	if got := firstFit(resources{1000, 1000}, nodes, nil, ready); got != "node1" {
		t.Errorf("an exact fit fits: %q", got)
	}
	if got := firstFit(resources{1001, 1}, nodes, nil, ready); got != "" {
		t.Errorf("too big: %q", got)
	}
	if got := firstFit(resources{1, 1}, nodes, nil, func(n string) bool { return n != "node1" }); got != "node2" {
		t.Errorf("node1 is not ready: %q", got)
	}
}

func TestNextAddr(t *testing.T) {
	if got, err := nextAddr("10.81.0.0/16", nil); err != nil || got != "10.81.0.2" {
		t.Errorf("first: %q, %v", got, err)
	}
	if got, err := nextAddr("10.81.0.0/16", map[string]bool{"10.81.0.2": true, "10.81.0.4": true}); err != nil || got != "10.81.0.3" {
		t.Errorf("lowest free: %q, %v", got, err)
	}
	if _, err := nextAddr("10.81.0.0/30", map[string]bool{"10.81.0.2": true}); err == nil {
		t.Error("the last address of the subnet is the broadcast")
	}
	if _, err := nextAddr("nonsense", nil); err == nil {
		t.Error("bad subnet must fail")
	}
}

func TestPassPlaces(t *testing.T) {
	m := &memState{
		nodes: twoNodes(),
		services: map[string]store.Service{
			"a":      {Name: "a", Revision: 1, DesiredAwake: true, State: stateCreating, Spec: specBytes(t, 600, 100, 80)},
			"b":      {Name: "b", Revision: 1, DesiredAwake: true, State: stateCreating, Spec: specBytes(t, 600, 100, 80)},
			"c":      {Name: "c", Revision: 1, DesiredAwake: true, State: stateCreating, Spec: specBytes(t, 400, 100, 80)},
			"big":    {Name: "big", Revision: 1, DesiredAwake: true, State: stateCreating, Spec: specBytes(t, 5000, 100, 80)},
			"asleep": {Name: "asleep", Revision: 1, DesiredAwake: false, State: stateAsleep, Spec: specBytes(t, 100, 100, 80)},
			"moved":  {Name: "moved", Revision: 1, DesiredAwake: false, DesiredNode: "node2", State: stateAsleep},
		},
	}
	if err := NewReconciler(m).Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][2]string{
		"a":   {"node1", "10.81.0.2"},
		"b":   {"node2", "10.82.0.2"},
		"c":   {"node1", "10.81.0.3"},
		"big": {"", ""},
	} {
		if got := m.services[name]; got.DesiredNode != want[0] || got.DesiredAddr != want[1] {
			t.Errorf("%s on %q at %q, want %v", name, got.DesiredNode, got.DesiredAddr, want)
		}
	}
	if got := m.services["asleep"]; got.DesiredNode != "" || got.DesiredAddr != "" {
		t.Errorf("what sleeps reserves nothing: %+v", got)
	}
	// A second pass, as after a restart of andend, keeps every placement.
	before := m.services["a"]
	if err := NewReconciler(m).Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if after := m.services["a"]; after.DesiredNode != before.DesiredNode || after.DesiredAddr != before.DesiredAddr {
		t.Errorf("placement changed: %+v", after)
	}
}

func TestPassPlacesMovedServiceInItsNodeSubnet(t *testing.T) {
	m := &memState{nodes: twoNodes(), services: map[string]store.Service{
		"a": {Name: "a", Revision: 1, DesiredAwake: true, DesiredNode: "node2", State: stateWaking, Spec: specBytes(t, 100, 100, 80)},
	}}
	if err := NewReconciler(m).Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.services["a"]; got.DesiredNode != "node2" || got.DesiredAddr != "10.82.0.2" {
		t.Fatalf("%+v", got)
	}
}

func TestPassProgramsEdge(t *testing.T) {
	m := &memState{nodes: twoNodes(), services: map[string]store.Service{
		"a": {Name: "a", Revision: 1, DesiredAwake: true, State: stateCreating, EdgePort: 30000, Spec: specBytes(t, 100, 100, 8080)},
	}}
	var scripts []string
	r := NewReconciler(m)
	r.edge = &Edge{ip: "10.80.0.2", apply: func(s string) error { scripts = append(scripts, s); return nil }}
	for range 2 {
		if err := r.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(scripts) != 1 {
		t.Fatalf("converged passes must not touch nft: %q", scripts)
	}
	if !strings.Contains(scripts[0], "add element ip anden_edge svc { 30000 : 10.81.0.2 . 8080 }") {
		t.Errorf("script: %s", scripts[0])
	}
}

func TestEdgeScript(t *testing.T) {
	a := edgeTarget{addr: "10.81.0.2", port: 8080}
	b := edgeTarget{addr: "10.82.0.2", port: 8080}
	create := edgeScript("10.80.0.2", nil, nil)
	for _, want := range []string{"delete table ip anden_edge", "type inet_service : ipv4_addr . inet_service",
		"ip daddr 10.80.0.2 dnat ip to tcp dport map @svc", "hook prerouting", "hook output"} {
		if !strings.Contains(create, want) {
			t.Errorf("create lacks %q:\n%s", want, create)
		}
	}
	if got := edgeScript("10.80.0.2", map[int32]edgeTarget{30000: a}, map[int32]edgeTarget{30000: a}); got != "" {
		t.Errorf("no change: %q", got)
	}
	got := edgeScript("10.80.0.2", map[int32]edgeTarget{30000: a, 30001: a}, map[int32]edgeTarget{30000: b, 30002: a})
	want := "delete element ip anden_edge svc { 30000 }\n" +
		"delete element ip anden_edge svc { 30001 }\n" +
		"add element ip anden_edge svc { 30000 : 10.82.0.2 . 8080 }\n" +
		"add element ip anden_edge svc { 30002 : 10.81.0.2 . 8080 }\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestToAssignments(t *testing.T) {
	got, err := toAssignments([]store.Service{{Name: "a", Version: 2, Generation: 3, DesiredAddr: "10.81.0.2", DesiredAwake: true, Spec: specBytes(t, 100, 64, 80)}})
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
	as := got[0]
	if as.GetInstanceId() != "a-3" || as.GetVersion() != 2 || as.GetGeneration() != 3 || as.GetAddr() != "10.81.0.2" || !as.GetAwake() || as.GetSpec().GetCpuMillis() != 100 {
		t.Fatalf("%v", as)
	}
}

func TestBlobHandler(t *testing.T) {
	registry := t.TempDir()
	hex := strings.Repeat("ab", 32)
	if err := os.MkdirAll(filepath.Join(registry, "blobs", "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(registry, "blobs", "sha256", hex), []byte("blob"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(blobHandler(registry))
	defer srv.Close()
	for path, want := range map[string]int{
		"/blobs/sha256/" + hex:                      http.StatusOK,
		"/blobs/sha256/" + strings.Repeat("cd", 32): http.StatusNotFound,
		"/blobs/sha256/nothex":                      http.StatusNotFound,
		"/blobs/sha256/..%2f..%2findex.json":        http.StatusNotFound,
		"/other":                                    http.StatusNotFound,
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s: %d, want %d", path, resp.StatusCode, want)
		}
	}
}

func TestApplyIntent(t *testing.T) {
	base := store.Service{DesiredNode: "node1", Generation: 2, DesiredAwake: true, State: stateActive}
	asleep := base
	if err := applyIntent(&asleep, "node1", 2, false); err != nil || asleep.DesiredAwake || asleep.State != stateSleeping {
		t.Fatalf("sleep: %v %+v", err, asleep)
	}
	woken := store.Service{DesiredNode: "node1", Generation: 2, State: stateAsleep}
	if err := applyIntent(&woken, "node1", 2, true); err != nil || !woken.DesiredAwake || woken.State != stateWaking || woken.Generation != 2 {
		t.Fatalf("wake: %v %+v", err, woken)
	}
	for name, s := range map[string]struct {
		node       string
		generation uint64
		awake      bool
	}{
		"other node":       {"node2", 2, false},
		"other generation": {"node1", 1, false},
		"already so":       {"node1", 2, true},
	} {
		got := base
		if err := applyIntent(&got, s.node, s.generation, s.awake); !errors.Is(err, errNothingToChange) || got.DesiredAwake != base.DesiredAwake || got.State != base.State {
			t.Errorf("%s: %v %+v", name, err, got)
		}
	}
}
