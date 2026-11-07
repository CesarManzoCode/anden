package controlplane

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	andenv1 "github.com/CesarManzoCode/anden/gen/anden/v1"
	"github.com/CesarManzoCode/anden/internal/dns"
	"github.com/CesarManzoCode/anden/internal/store"
)

func (m *memState) Deployments(context.Context) ([]store.Deployment, error) {
	var out []store.Deployment
	for _, d := range m.deploys {
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b store.Deployment) int { return strings.Compare(a.Service, b.Service) })
	return out, nil
}

func (m *memState) SetDeployAddr(_ context.Context, service string, generation int64, addr string) (bool, error) {
	d, ok := m.deploys[service]
	if !ok || d.Generation != generation || d.Addr != "" {
		return false, nil
	}
	d.Addr = addr
	m.deploys[service] = d
	return true, nil
}

func (m *memState) SwitchDeploy(_ context.Context, service string, generation int64, from, phase string, now time.Time) (bool, error) {
	d, ok := m.deploys[service]
	if !ok || d.Generation != generation || d.Phase != from || d.Addr == "" {
		return false, nil
	}
	s, ok := m.services[service]
	if !ok || s.Deleted || s.Generation != d.OldGeneration {
		return false, nil
	}
	s.Spec, s.Version, s.Generation, s.DesiredNode, s.DesiredAddr, s.DesiredAwake = d.Spec, d.Version, d.Generation, d.Node, d.Addr, true
	s.Revision++
	m.services[service] = s
	d.Phase, d.Since = phase, now
	m.deploys[service] = d
	return true, nil
}

func (m *memState) SetDeployPhase(_ context.Context, service string, generation int64, from, to string, now time.Time) (bool, error) {
	d, ok := m.deploys[service]
	if !ok || d.Generation != generation || d.Phase != from {
		return false, nil
	}
	d.Phase, d.Since = to, now
	m.deploys[service] = d
	return true, nil
}

func (m *memState) EndDeploy(_ context.Context, service string, generation int64) error {
	if d, ok := m.deploys[service]; ok && d.Generation == generation {
		delete(m.deploys, service)
	}
	return nil
}

var deployStart = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

func versionSpec(t *testing.T, version string) []byte {
	t.Helper()
	raw, err := proto.Marshal(&andenv1.ServiceSpec{
		Name: "web", Port: 8080, CpuMillis: 100, MemoryMib: 64, Env: map[string]string{"VERSION": version},
		IdleSeconds: proto.Uint32(60), HealthcheckTimeoutSeconds: proto.Uint32(10),
		OverlapSeconds: proto.Uint32(5), DrainingSeconds: proto.Uint32(2),
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func webInstance(generation int64, addr, state string) store.Instance {
	return store.Instance{ID: InstanceID("web", generation), Service: "web", Node: "node1", Version: generation,
		Generation: generation, State: state, Addr: addr}
}

// rolloutState is web v1 running on node1 at 10.81.0.2, with a deploy of v2
// that DeployService just recorded.
func rolloutState(t *testing.T) *memState {
	return &memState{
		nodes: twoNodes(),
		services: map[string]store.Service{"web": {
			Name: "web", Spec: versionSpec(t, "v1"), Version: 1, Generation: 1, DesiredAwake: true, DesiredNode: "node1",
			DesiredAddr: "10.81.0.2", Revision: 4, State: stateActive, Node: "node1", Addr: "10.81.0.2", EdgePort: 30001,
		}},
		instances: []store.Instance{webInstance(1, "10.81.0.2", instRunning)},
		deploys: map[string]store.Deployment{"web": {
			Service: "web", Phase: phaseStarting, Spec: versionSpec(t, "v2"), Version: 2, Generation: 2, Node: "node1",
			OldSpec: versionSpec(t, "v1"), OldVersion: 1, OldGeneration: 1, OldNode: "node1", OldAddr: "10.81.0.2",
			Since: deployStart,
		}},
	}
}

// rolloutReconciler is a new andend over m: its own edge, empty until its first
// pass, its own DNS records and a clock that reads *now.
func rolloutReconciler(m *memState, scripts *[]string, now *time.Time) *Reconciler {
	r := NewReconciler(m)
	r.edge = &Edge{ip: "10.80.0.2", apply: func(s string) error { *scripts = append(*scripts, s); return nil }}
	r.records = dns.NewRecords()
	r.now = func() time.Time { return *now }
	return r
}

func pass(t *testing.T, r *Reconciler) {
	t.Helper()
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// heartbeat is what node1's agent would receive.
func heartbeat(t *testing.T, m *memState) map[string]*andenv1.Assignment {
	t.Helper()
	var placed []store.Service
	for _, s := range m.services {
		if s.DesiredNode == "node1" && s.DesiredAddr != "" && !s.Deleted {
			placed = append(placed, s)
		}
	}
	as, err := toAssignments(placed)
	if err != nil {
		t.Fatal(err)
	}
	deploys, _ := m.Deployments(context.Background())
	if as, err = deployAssignments("node1", as, deploys); err != nil {
		t.Fatal(err)
	}
	out := map[string]*andenv1.Assignment{}
	for _, a := range as {
		if out[a.GetInstanceId()] != nil {
			t.Fatalf("instance %s assigned twice", a.GetInstanceId())
		}
		out[a.GetInstanceId()] = a
	}
	return out
}

func edgeLeadsTo(scripts []string, addr string) bool {
	return strings.Contains(scripts[len(scripts)-1], "add element ip anden_edge svc { 30001 : "+addr+" . 8080 }")
}

func setInstances(m *memState, instances ...store.Instance) { m.instances = instances }

func TestRolloutSucceeds(t *testing.T) {
	m := rolloutState(t)
	var scripts []string
	now := deployStart
	r := rolloutReconciler(m, &scripts, &now)

	pass(t, r)
	if got := m.deploys["web"].Addr; got != "10.81.0.3" {
		t.Fatalf("address of v2: %q", got)
	}
	if !edgeLeadsTo(scripts, "10.81.0.2") {
		t.Fatalf("the edge left v1 before v2 was healthy: %s", scripts[len(scripts)-1])
	}
	hb := heartbeat(t, m)
	v1, v2 := hb["web-1"], hb["web-2"]
	if v1 == nil || !v1.GetAwake() || v1.GetSpec().GetDrainingSeconds() != 2 || v1.GetSpec().GetIdleSeconds() != 0 {
		t.Fatalf("v1 while v2 starts: %v", v1)
	}
	if v2 == nil || !v2.GetAwake() || v2.GetAddr() != "10.81.0.3" || v2.GetVersion() != 2 || v2.GetSpec().DrainingSeconds != nil {
		t.Fatalf("v2 while it starts: %v", v2)
	}

	setInstances(m, webInstance(1, "10.81.0.2", instRunning), webInstance(2, "10.81.0.3", instStarting))
	now = now.Add(time.Second)
	pass(t, r)
	if m.deploys["web"].Phase != phaseStarting || m.services["web"].Version != 1 {
		t.Fatalf("switched before the healthcheck: %+v", m.deploys["web"])
	}

	setInstances(m, webInstance(1, "10.81.0.2", instRunning), webInstance(2, "10.81.0.3", instRunning))
	now = now.Add(time.Second)
	switchedAt := now
	pass(t, r)
	svc := m.services["web"]
	if svc.Version != 2 || svc.Generation != 2 || svc.DesiredAddr != "10.81.0.3" || m.deploys["web"].Phase != phaseOverlap {
		t.Fatalf("after the healthcheck: %+v %+v", svc, m.deploys["web"])
	}
	if !edgeLeadsTo(scripts, "10.81.0.3") {
		t.Fatalf("edge after the switch: %s", scripts[len(scripts)-1])
	}
	if addr, ok := r.records.Lookup("web"); !ok || addr.String() != "10.81.0.3" {
		t.Fatalf("DNS after the switch: %v %v", addr, ok)
	}
	hb = heartbeat(t, m)
	if v1 := hb["web-1"]; v1 == nil || !v1.GetAwake() || v1.GetAddr() != "10.81.0.2" || v1.GetSpec().GetDrainingSeconds() != 2 {
		t.Fatalf("v1 during the overlap: %v", v1)
	}
	if v2 := hb["web-2"]; v2 == nil || !v2.GetAwake() || v2.GetSpec().DrainingSeconds != nil {
		t.Fatalf("v2 during the overlap: %v", v2)
	}

	now = switchedAt.Add(4 * time.Second)
	pass(t, r)
	if m.deploys["web"].Phase != phaseOverlap {
		t.Fatalf("draining before overlap_seconds: %+v", m.deploys["web"])
	}
	now = switchedAt.Add(5 * time.Second)
	pass(t, r)
	if m.deploys["web"].Phase != phaseDraining {
		t.Fatalf("not draining after overlap_seconds: %+v", m.deploys["web"])
	}
	// No longer assigned, v1 gets SIGTERM from its agent, and SIGKILL after
	// the 2 s it was given.
	if hb = heartbeat(t, m); hb["web-1"] != nil || hb["web-2"] == nil {
		t.Fatalf("assignments while draining: %v", hb)
	}

	now = now.Add(2 * time.Second)
	pass(t, r)
	if _, ok := m.deploys["web"]; !ok {
		t.Fatalf("ended while v1 is still reported")
	}
	setInstances(m, webInstance(2, "10.81.0.3", instRunning))
	pass(t, r)
	if _, ok := m.deploys["web"]; ok {
		t.Fatalf("still under way after draining: %+v", m.deploys["web"])
	}
	if svc := m.services["web"]; svc.State != stateActive || svc.Addr != "10.81.0.3" {
		t.Fatalf("observed at the end: %+v", svc)
	}
}

func TestRolloutKeepsOldVersionWhenHealthFails(t *testing.T) {
	for name, failure := range map[string]func(m *memState, now *time.Time){
		"instance failed": func(m *memState, _ *time.Time) {
			setInstances(m, webInstance(1, "10.81.0.2", instRunning), webInstance(2, "10.81.0.3", instFailed))
		},
		"healthcheck timeout": func(m *memState, now *time.Time) {
			setInstances(m, webInstance(1, "10.81.0.2", instRunning), webInstance(2, "10.81.0.3", instStarting))
			*now = deployStart.Add(10 * time.Second)
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := rolloutState(t)
			var scripts []string
			now := deployStart
			r := rolloutReconciler(m, &scripts, &now)
			pass(t, r)
			failure(m, &now)
			pass(t, r)
			if _, ok := m.deploys["web"]; ok {
				t.Fatalf("deploy still under way: %+v", m.deploys["web"])
			}
			svc := m.services["web"]
			if svc.Version != 1 || svc.Generation != 1 || svc.DesiredAddr != "10.81.0.2" || svc.State != stateActive {
				t.Fatalf("service after the failed deploy: %+v", svc)
			}
			for _, s := range scripts {
				if strings.Contains(s, "10.81.0.3") {
					t.Fatalf("the edge was touched: %s", s)
				}
			}
			if addr, _ := r.records.Lookup("web"); addr.String() != "10.81.0.2" {
				t.Fatalf("DNS: %v", addr)
			}
			// v2 is no longer assigned, so its agent destroys it; v1 is back
			// to its own idle window and the usual grace.
			hb := heartbeat(t, m)
			v1 := hb["web-1"]
			if hb["web-2"] != nil || v1 == nil || v1.GetSpec().GetIdleSeconds() != 60 || v1.GetSpec().DrainingSeconds != nil {
				t.Fatalf("assignments after the failed deploy: %v", hb)
			}
		})
	}
}

func TestRolloutResumesAfterRestart(t *testing.T) {
	m := rolloutState(t)
	var scripts []string
	now := deployStart
	pass(t, rolloutReconciler(m, &scripts, &now))
	if m.deploys["web"].Addr == "" {
		t.Fatalf("no address for v2")
	}

	// andend restarts while v2 starts: the new one switches.
	setInstances(m, webInstance(1, "10.81.0.2", instRunning), webInstance(2, "10.81.0.3", instRunning))
	now = now.Add(time.Second)
	scripts = nil
	pass(t, rolloutReconciler(m, &scripts, &now))
	if m.deploys["web"].Phase != phaseOverlap || m.services["web"].DesiredAddr != "10.81.0.3" {
		t.Fatalf("after the first restart: %+v", m.deploys["web"])
	}

	// andend restarts during the overlap: the next one recreates the edge with
	// v2, and goes on with the time the overlap began, not its own start.
	now = now.Add(5 * time.Second)
	scripts = nil
	r := rolloutReconciler(m, &scripts, &now)
	pass(t, r)
	if !strings.Contains(scripts[0], "delete table ip anden_edge") || !edgeLeadsTo(scripts, "10.81.0.3") {
		t.Fatalf("edge of the restarted andend: %q", scripts)
	}
	if m.deploys["web"].Phase != phaseDraining {
		t.Fatalf("after the second restart: %+v", m.deploys["web"])
	}
	setInstances(m, webInstance(2, "10.81.0.3", instRunning))
	now = now.Add(2 * time.Second)
	pass(t, r)
	if _, ok := m.deploys["web"]; ok {
		t.Fatalf("not ended: %+v", m.deploys["web"])
	}
}

func TestDeployOnSleepingService(t *testing.T) {
	active := store.Service{DesiredAwake: true, State: stateActive, DesiredNode: "node1", DesiredAddr: "10.81.0.2", Spec: versionSpec(t, "v1")}
	singleton := active
	raw, err := proto.Marshal(&andenv1.ServiceSpec{Name: "web", Singleton: true})
	if err != nil {
		t.Fatal(err)
	}
	singleton.Spec = raw
	asleep := store.Service{State: stateAsleep, DesiredNode: "node1", DesiredAddr: "10.81.0.2"}
	sleeping := store.Service{State: stateSleeping, DesiredNode: "node1", DesiredAddr: "10.81.0.2"}
	creating := store.Service{DesiredAwake: true, State: stateCreating}
	if !rolloutWanted(active) {
		t.Errorf("an active service must roll out")
	}
	for name, svc := range map[string]store.Service{"asleep": asleep, "sleeping": sleeping, "creating": creating, "singleton": singleton} {
		if rolloutWanted(svc) {
			t.Errorf("%s: a deploy must only change the desired version", name)
		}
	}

	// A service that went to sleep while v2 started has nothing to overlap:
	// v2 becomes the awake one and v1 drains at once.
	m := rolloutState(t)
	svc := m.services["web"]
	svc.DesiredAwake, svc.State = false, stateSleeping
	m.services["web"] = svc
	var scripts []string
	now := deployStart
	r := rolloutReconciler(m, &scripts, &now)
	pass(t, r)
	setInstances(m, webInstance(1, "10.81.0.2", instRunning), webInstance(2, "10.81.0.3", instRunning))
	pass(t, r)
	if got := m.services["web"]; !got.DesiredAwake || got.Version != 2 || m.deploys["web"].Phase != phaseDraining {
		t.Fatalf("after the switch: %+v %+v", got, m.deploys["web"])
	}
}

func TestNextDeployStepGivesUp(t *testing.T) {
	m := rolloutState(t)
	d := m.deploys["web"]
	spec := &andenv1.ServiceSpec{HealthcheckTimeoutSeconds: proto.Uint32(10), OverlapSeconds: proto.Uint32(5), DrainingSeconds: proto.Uint32(2)}
	moved := m.services["web"]
	moved.Generation = 2
	if got := nextDeployStep(d, spec, moved, true, nil, deployStart); got != stepEnd {
		t.Errorf("service in another generation: %v", got)
	}
	if got := nextDeployStep(d, spec, store.Service{}, false, nil, deployStart); got != stepEnd {
		t.Errorf("service gone: %v", got)
	}
	d.Phase = phaseDraining
	old := []store.Instance{webInstance(1, "10.81.0.2", instRunning)}
	if got := nextDeployStep(d, spec, m.services["web"], true, old, deployStart.Add(2*time.Second)); got != stepWait {
		t.Errorf("old instance still reported: %v", got)
	}
	if got := nextDeployStep(d, spec, m.services["web"], true, old, deployStart.Add(2*time.Second+drainLimit)); got != stepEnd {
		t.Errorf("node that never reports again: %v", got)
	}
}
