package controlplane

import (
	"context"
	"testing"

	"github.com/CesarManzoCode/anden/internal/store"
)

// memState is an in-memory State with the same revision rules as the store.
type memState struct {
	services  map[string]store.Service
	instances []store.Instance
}

func (m *memState) ListServices(_ context.Context, includeDeleted bool) ([]store.Service, error) {
	var out []store.Service
	for _, s := range m.services {
		if includeDeleted || !s.Deleted {
			out = append(out, s)
		}
	}
	return out, nil
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
