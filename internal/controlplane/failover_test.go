package controlplane

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	andenv1 "github.com/CesarManzoCode/anden/gen/anden/v1"
	"github.com/CesarManzoCode/anden/internal/store"
)

func (m *memState) Relocate(_ context.Context, name string, revision int64, from, to string) (bool, error) {
	s, ok := m.services[name]
	if !ok || s.Deleted || s.Revision != revision || s.DesiredNode != from {
		return false, nil
	}
	s.DesiredNode, s.DesiredAddr = to, ""
	s.Generation++
	s.Revision++
	m.services[name] = s
	return true, nil
}

func TestLivenessDeclaresDeadAfterTwiceTheTTL(t *testing.T) {
	clock := time.Unix(1000, 0)
	l := newLiveness(func() time.Time { return clock })
	if deadAfter != 4*time.Second || leaseTTL != 2*time.Second {
		t.Fatalf("D08 timing changed: TTL %s, dead after %s", leaseTTL, deadAfter)
	}
	// Never heard from since the start: it counts from the start.
	clock = clock.Add(deadAfter - time.Millisecond)
	if !l.ready("node1") {
		t.Fatal("a node not heard from since the start is dead before 4 s")
	}
	clock = clock.Add(time.Millisecond)
	if l.ready("node1") {
		t.Fatal("a node not heard from in 4 s is still ready")
	}
	l.touch("node1")
	clock = clock.Add(deadAfter - time.Millisecond)
	if !l.ready("node1") {
		t.Fatal("dead before 4 s after its last heartbeat")
	}
	frozen := l.snapshot()
	clock = clock.Add(time.Millisecond)
	if l.ready("node1") {
		t.Fatal("still ready 4 s after its last heartbeat")
	}
	if !frozen("node1") {
		t.Fatal("a snapshot must not follow the clock")
	}
}

func TestCheckEpochRefusesOtherEpochs(t *testing.T) {
	if err := checkEpoch("node1", 3, 3); err != nil {
		t.Fatalf("current epoch refused: %v", err)
	}
	for _, got := range []uint64{0, 2, 4} {
		if err := checkEpoch("node1", 3, got); status.Code(err) != codes.FailedPrecondition {
			t.Errorf("epoch %d against 3: %v", got, err)
		}
	}
}

func singletonAssignment(t *testing.T, name string, generation uint64, singleton bool) *andenv1.Assignment {
	t.Helper()
	return &andenv1.Assignment{InstanceId: InstanceID(name, int64(generation)), Generation: generation,
		Spec: &andenv1.ServiceSpec{Name: name, Singleton: singleton}}
}

func TestHoldSingletons(t *testing.T) {
	ready := func(n string) bool { return n != "node3" }
	instance := func(service, node string, generation int64, state string) store.Instance {
		return store.Instance{ID: InstanceID(service, generation), Service: service, Node: node, Generation: generation, State: state}
	}
	cases := []struct {
		name      string
		instances []store.Instance
		held      bool
	}{
		{"nothing elsewhere", nil, false},
		{"old generation running on a live node", []store.Instance{instance("w", "node1", 1, instRunning)}, true},
		{"old generation starting on a live node", []store.Instance{instance("w", "node1", 1, instStarting)}, true},
		{"old generation on a dead node", []store.Instance{instance("w", "node3", 1, instRunning)}, false},
		{"old generation failed", []store.Instance{instance("w", "node1", 1, instFailed)}, false},
		{"same generation on this node", []store.Instance{instance("w", "node2", 2, instRunning)}, false},
		{"another service", []store.Instance{instance("x", "node1", 1, instRunning)}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := []*andenv1.Assignment{singletonAssignment(t, "w", 2, true), singletonAssignment(t, "plain", 1, false)}
			got := holdSingletons("node2", in, c.instances, ready)
			if held := len(got) == 1; held != c.held {
				t.Fatalf("held %v, want %v: %v", held, c.held, got)
			}
			if got[len(got)-1].GetSpec().GetName() != "plain" {
				t.Fatal("a service that is not a singleton was held")
			}
		})
	}
	// Not a singleton: two instances may overlap, as in a deploy.
	plain := []*andenv1.Assignment{singletonAssignment(t, "w", 2, false)}
	if got := holdSingletons("node2", plain, []store.Instance{instance("w", "node1", 1, instRunning)}, ready); len(got) != 1 {
		t.Fatal("held a service that is not a singleton")
	}
}

func relocationState(t *testing.T) *memState {
	sized := func(cpu uint32, singleton bool) []byte {
		raw, err := proto.Marshal(&andenv1.ServiceSpec{CpuMillis: cpu, MemoryMib: 100, Port: 8080, Singleton: singleton})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	nodes := append(twoNodes(), store.Node{Name: "node3", Subnet: "10.83.0.0/16", CPUMillis: 1000, MemoryMiB: 1000})
	return &memState{nodes: nodes, services: map[string]store.Service{
		"a": {Name: "a", Revision: 1, Generation: 1, DesiredAwake: true, DesiredNode: "node1", DesiredAddr: "10.81.0.2",
			State: stateActive, Node: "node1", Addr: "10.81.0.2", EdgePort: 30000, Spec: sized(300, false)},
		"b": {Name: "b", Revision: 1, Generation: 1, DesiredAwake: true, DesiredNode: "node1", DesiredAddr: "10.81.0.3",
			State: stateActive, Node: "node1", Addr: "10.81.0.3", EdgePort: 30001, Spec: sized(600, true)},
		"c": {Name: "c", Revision: 1, Generation: 4, DesiredNode: "node1", DesiredAddr: "10.81.0.4",
			State: stateAsleep, Node: "node1", Addr: "10.81.0.4", EdgePort: 30002, Spec: sized(100, false)},
		"d": {Name: "d", Revision: 1, Generation: 1, DesiredAwake: true, DesiredNode: "node2", DesiredAddr: "10.82.0.2",
			State: stateActive, Node: "node2", Addr: "10.82.0.2", EdgePort: 30003, Spec: sized(400, false)},
	}, instances: []store.Instance{
		{ID: "a-1", Service: "a", Node: "node1", Generation: 1, State: instRunning, Addr: "10.81.0.2"},
		{ID: "b-1", Service: "b", Node: "node1", Generation: 1, State: instRunning, Addr: "10.81.0.3"},
		{ID: "d-1", Service: "d", Node: "node2", Generation: 1, State: instRunning, Addr: "10.82.0.2"},
	}}
}

func TestPassKeepsServicesOfLiveNodes(t *testing.T) {
	m := relocationState(t)
	before := m.services["a"]
	if err := NewReconciler(m).Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.services["a"]; got.DesiredNode != "node1" || got.Generation != before.Generation {
		t.Fatalf("a moved while node1 is ready: %+v", got)
	}
}

func TestPassRelocatesServicesOfDeadNode(t *testing.T) {
	m := relocationState(t)
	r := NewReconciler(m)
	r.ready = func(n string) bool { return n != "node1" }
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Decreasing order: b (600) takes the best fit, node2 beside d (400);
	// a (300) no longer fits there and goes to node3. In name order a would
	// have taken node2 and b node3.
	for name, node := range map[string]string{"a": "node3", "b": "node2"} {
		got := m.services[name]
		subnet := map[string]string{"node2": "10.82.", "node3": "10.83."}[node]
		if got.DesiredNode != node || !strings.HasPrefix(got.DesiredAddr, subnet) || got.Generation != 2 || !got.DesiredAwake {
			t.Errorf("%s after the relocation: %+v", name, got)
		}
		// What node1 reported is not taken as live: the service is where it
		// will run, waking there.
		if got.Node != node || got.State != stateWaking {
			t.Errorf("%s observed: node %q state %s", name, got.Node, got.State)
		}
	}
	// Asleep: a new home and an address there, without waking, in a new
	// generation; best fit over what the awake services reserve.
	c := m.services["c"]
	if c.DesiredAwake || c.DesiredNode == "node1" || c.DesiredNode == "" || c.Generation != 5 || strings.HasPrefix(c.DesiredAddr, "10.81.") ||
		c.DesiredAddr == "" || c.State != stateAsleep {
		t.Errorf("c after the relocation: %+v", c)
	}
	if d := m.services["d"]; d.DesiredNode != "node2" || d.DesiredAddr != "10.82.0.2" || d.Generation != 1 {
		t.Errorf("d is on a live node and must stay: %+v", d)
	}
	// The new home receives b in its new generation, awake, and nothing
	// still runs it elsewhere on a live node.
	as, err := toAssignments([]store.Service{m.services["b"]})
	if err != nil || as[0].GetInstanceId() != "b-2" || !as[0].GetAwake() {
		t.Fatalf("assignment of b: %v %v", as, err)
	}
	instances, _ := m.Instances(context.Background())
	if got := holdSingletons("node2", as, instances, r.ready); len(got) != 1 {
		t.Fatal("the singleton b was held although its old node is dead")
	}
	// A second pass changes nothing more.
	again := m.services["a"]
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.services["a"]; got.Generation != again.Generation || got.DesiredAddr != again.DesiredAddr {
		t.Errorf("a moved again: %+v", got)
	}
}

func TestPassRelocationLosesAgainstNewerRevision(t *testing.T) {
	m := relocationState(t)
	r := NewReconciler(m)
	r.ready = func(n string) bool { return n != "node1" }
	svc := m.services["a"]
	if err := r.moveOff(context.Background(), &store.Service{Name: "a", Revision: svc.Revision - 1, DesiredNode: "node1"}, ""); err != nil {
		t.Fatal(err)
	}
	if got := m.services["a"]; got.DesiredNode != "node1" || got.Generation != 1 {
		t.Fatalf("a stale relocation was written: %+v", got)
	}
}

func TestLiveInstancePrefersTheNewGeneration(t *testing.T) {
	got := liveInstance([]store.Instance{
		{ID: "a-1", Version: 1, Generation: 1, State: instRunning, Node: "node1"},
		{ID: "a-2", Version: 1, Generation: 2, State: instRunning, Node: "node2"},
	})
	if got == nil || got.ID != "a-2" {
		t.Fatalf("live instance %+v", got)
	}
}
