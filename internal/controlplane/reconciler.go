// Package controlplane is andend: the gRPC API, the AgentControl service for
// node agents, the blob server, the edge and the level reconciler that compares
// desired state with what agents report (contract D02, D21 and D23).
package controlplane

import (
	"context"
	"fmt"
	"log"
	"net/netip"
	"time"

	"google.golang.org/protobuf/proto"

	andenv1 "github.com/CesarManzoCode/anden/gen/anden/v1"
	"github.com/CesarManzoCode/anden/internal/dns"
	"github.com/CesarManzoCode/anden/internal/scheduler"
	"github.com/CesarManzoCode/anden/internal/store"
)

// States are stored with their proto enum names.
var (
	stateCreating = andenv1.ServiceState_SERVICE_STATE_CREATING.String()
	stateActive   = andenv1.ServiceState_SERVICE_STATE_ACTIVE.String()
	stateSleeping = andenv1.ServiceState_SERVICE_STATE_SLEEPING.String()
	stateAsleep   = andenv1.ServiceState_SERVICE_STATE_ASLEEP.String()
	stateWaking   = andenv1.ServiceState_SERVICE_STATE_WAKING.String()
	stateFailed   = andenv1.ServiceState_SERVICE_STATE_FAILED.String()

	instStarting = andenv1.InstanceState_INSTANCE_STATE_STARTING.String()
	instRunning  = andenv1.InstanceState_INSTANCE_STATE_RUNNING.String()
	instFailed   = andenv1.InstanceState_INSTANCE_STATE_FAILED.String()
)

// State is the part of the store the reconciler needs.
type State interface {
	ListServices(ctx context.Context, includeDeleted bool) ([]store.Service, error)
	Instances(ctx context.Context) ([]store.Instance, error)
	ListNodes(ctx context.Context) ([]store.Node, error)
	SetPlacement(ctx context.Context, name string, revision int64, node, addr string) (bool, error)
	SetObserved(ctx context.Context, name string, revision int64, obs store.Observed) (bool, error)
	Purge(ctx context.Context, name string, revision int64) error
	Deployments(ctx context.Context) ([]store.Deployment, error)
	SetDeployAddr(ctx context.Context, service string, generation int64, addr string) (bool, error)
	SwitchDeploy(ctx context.Context, service string, generation int64, from, phase string, now time.Time) (bool, error)
	SetDeployPhase(ctx context.Context, service string, generation int64, from, to string, now time.Time) (bool, error)
	EndDeploy(ctx context.Context, service string, generation int64) error
}

// Reconciler is level based: each pass reads the whole desired and reported
// state, places what has no node, programs the edge and writes the observed
// state that follows. Agents act on the placement when they read it in their
// heartbeat, and a new andend resumes from the same durable rows, so it adopts
// the running instances instead of starting them again.
type Reconciler struct {
	state State
	kick  chan struct{}
	// ready says whether a node can take new instances.
	ready func(node string) bool
	// edge is nil when andend runs without an edge address.
	edge *Edge
	// records are the names the DNS answers (D22); nil without a DNS.
	records *dns.Records
	// now is the wall clock: the phases of a deployment are timed against
	// instants stored in Postgres, which outlive this process.
	now func() time.Time
}

func NewReconciler(state State) *Reconciler {
	return &Reconciler{state: state, kick: make(chan struct{}, 1), ready: func(string) bool { return true }, now: time.Now}
}

// Kick asks for a pass without waiting for the next tick.
func (r *Reconciler) Kick() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// Run makes a pass every interval, or sooner after a Kick, until ctx ends.
func (r *Reconciler) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := r.Pass(ctx); err != nil && ctx.Err() == nil {
			log.Printf("reconcile: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-r.kick:
		}
	}
}

// Pass reconciles every service once.
func (r *Reconciler) Pass(ctx context.Context) error {
	services, err := r.state.ListServices(ctx, true)
	if err != nil {
		return err
	}
	instances, err := r.state.Instances(ctx)
	if err != nil {
		return err
	}
	deploys, err := r.state.Deployments(ctx)
	if err != nil {
		return err
	}
	changed, err := r.rollout(ctx, services, instances, deploys)
	if err != nil {
		return err
	}
	// A switch changed the address of a service: the edge and the DNS of
	// this same pass must follow it.
	if changed {
		if services, err = r.state.ListServices(ctx, true); err != nil {
			return err
		}
		if deploys, err = r.state.Deployments(ctx); err != nil {
			return err
		}
	}
	if err := r.place(ctx, services, deploys); err != nil {
		return err
	}
	if r.records != nil {
		r.records.Replace(dnsRecords(services))
	}
	byService := map[string][]store.Instance{}
	for _, in := range instances {
		byService[in.Service] = append(byService[in.Service], in)
	}
	for _, svc := range services {
		obs, purge := Decide(svc, byService[svc.Name])
		if purge {
			err = r.state.Purge(ctx, svc.Name, svc.Revision)
		} else if obs != (store.Observed{State: svc.State, Node: svc.Node, Addr: svc.Addr}) {
			// A false result means the API changed the service meanwhile;
			// the next pass sees the new revision.
			_, err = r.state.SetObserved(ctx, svc.Name, svc.Revision, obs)
		}
		if err != nil {
			return err
		}
	}
	if r.edge == nil {
		return nil
	}
	want, err := edgeTargets(services)
	if err != nil {
		return err
	}
	return r.edge.Sync(want)
}

func specOf(svc store.Service) (*andenv1.ServiceSpec, error) {
	spec := &andenv1.ServiceSpec{}
	if err := proto.Unmarshal(svc.Spec, spec); err != nil {
		return nil, fmt.Errorf("stored spec of %s: %w", svc.Name, err)
	}
	return spec, nil
}

// place gives a node and an address to every awake service without an address,
// and an address of its home to a sleeping one that moved, and records them in
// services. The services without a node are placed together as one batch by
// the scheduler of D07, over the ready nodes and on top of what the awake
// services with a node already reserve. A service that fits nowhere waits for
// a later pass; lost revision races too. The new instance of a deployment gets
// an address of its node the same way, and the addresses of both instances of
// a deployment stay taken until it ends.
func (r *Reconciler) place(ctx context.Context, services []store.Service, deploys []store.Deployment) error {
	nodes, err := r.state.ListNodes(ctx)
	if err != nil {
		return err
	}
	subnets := map[string]string{}
	problem := scheduler.Problem{}
	for _, n := range nodes {
		subnets[n.Name] = n.Subnet
		if r.ready(n.Name) {
			problem.Nodes = append(problem.Nodes, scheduler.Node{Name: n.Name, CPUMillis: int64(n.CPUMillis), MemMiB: int64(n.MemoryMiB)})
		}
	}
	used := map[string]bool{}
	for _, d := range deploys {
		used[d.Addr], used[d.OldAddr] = true, true
	}
	delete(used, "")
	for _, svc := range services {
		if svc.DesiredAddr != "" {
			used[svc.DesiredAddr] = true
		}
		// D07: only what is awake reserves; what sleeps reserves nothing.
		if svc.Deleted || !svc.DesiredAwake || (svc.DesiredNode == "" && svc.DesiredAddr != "") {
			continue
		}
		spec, err := specOf(svc)
		if err != nil {
			return err
		}
		problem.Services = append(problem.Services, scheduler.Service{
			Name:              svc.Name,
			CPUMillis:         int64(spec.GetCpuMillis()),
			MemMiB:            int64(spec.GetMemoryMib()),
			AntiAffinityGroup: spec.GetAntiAffinityGroup(),
			Node:              svc.DesiredNode,
		})
	}
	placement := scheduler.Solve(problem)
	for i := range services {
		svc := &services[i]
		// What sleeps is placed only once it has a home: MoveService gives
		// it one, and its address must follow there (D22).
		if svc.Deleted || svc.DesiredAddr != "" || (!svc.DesiredAwake && svc.DesiredNode == "") {
			continue
		}
		node := svc.DesiredNode
		if node == "" {
			node = placement[svc.Name]
		}
		subnet, known := subnets[node]
		if !known {
			continue
		}
		addr, err := nextAddr(subnet, used)
		if err != nil {
			return err
		}
		ok, err := r.state.SetPlacement(ctx, svc.Name, svc.Revision, node, addr)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		svc.DesiredNode, svc.DesiredAddr = node, addr
		used[addr] = true
	}
	for _, d := range deploys {
		subnet, known := subnets[d.Node]
		if d.Phase != phaseStarting || d.Addr != "" || !known {
			continue
		}
		addr, err := nextAddr(subnet, used)
		if err != nil {
			return err
		}
		ok, err := r.state.SetDeployAddr(ctx, d.Service, d.Generation, addr)
		if err != nil {
			return err
		}
		if ok {
			used[addr] = true
		}
	}
	return nil
}

// edgeTargets is the map of the edge: every live service with an address has
// its port, awake or not, so that its edge port does not change while it sleeps.
func edgeTargets(services []store.Service) (map[int32]edgeTarget, error) {
	want := map[int32]edgeTarget{}
	for _, svc := range services {
		if svc.Deleted || svc.DesiredAddr == "" {
			continue
		}
		spec, err := specOf(svc)
		if err != nil {
			return nil, err
		}
		want[svc.EdgePort] = edgeTarget{addr: svc.DesiredAddr, port: spec.GetPort()}
	}
	return want, nil
}

// dnsRecords is the address of every live service that has one, awake or not:
// a sleeping service resolves to its reserved address, which wakes it (D22).
func dnsRecords(services []store.Service) map[string]netip.Addr {
	out := map[string]netip.Addr{}
	for _, svc := range services {
		if svc.Deleted || svc.DesiredAddr == "" {
			continue
		}
		if addr, err := netip.ParseAddr(svc.DesiredAddr); err == nil {
			out[svc.Name] = addr
		}
	}
	return out
}

// InstanceID names the instance of a generation: <service>-<generation>.
func InstanceID(service string, generation int64) string {
	return fmt.Sprintf("%s-%d", service, generation)
}

// toAssignments is what a node's agent receives for the services placed on it.
// draining_seconds goes only to the old instance of a deploy (deployAssignments):
// any other instance that stops gets the agent's usual grace.
func toAssignments(services []store.Service) ([]*andenv1.Assignment, error) {
	out := make([]*andenv1.Assignment, 0, len(services))
	for _, svc := range services {
		spec, err := specOf(svc)
		if err != nil {
			return nil, err
		}
		spec.DrainingSeconds = nil
		out = append(out, &andenv1.Assignment{
			InstanceId: InstanceID(svc.Name, svc.Generation),
			Spec:       spec,
			Version:    uint64(svc.Version),
			Generation: uint64(svc.Generation),
			Addr:       svc.DesiredAddr,
			Awake:      svc.DesiredAwake,
		})
	}
	return out, nil
}

// liveInstance returns the running instance of the highest version, or nil.
func liveInstance(instances []store.Instance) *store.Instance {
	var live *store.Instance
	for i := range instances {
		in := &instances[i]
		if in.State == instRunning && (live == nil || in.Version > live.Version) {
			live = in
		}
	}
	return live
}

// Decide returns the observed state of svc given the instances reported for
// it, and whether a deleted service has nothing left and can be purged.
func Decide(svc store.Service, instances []store.Instance) (store.Observed, bool) {
	alive, failed := false, false
	for _, in := range instances {
		switch in.State {
		case instRunning, instStarting:
			alive = true
		case instFailed:
			failed = failed || (in.Version == svc.Version && in.Generation == svc.Generation)
		}
	}
	obs := store.Observed{State: svc.State, Node: svc.Node, Addr: svc.Addr}
	if svc.Deleted {
		return obs, !alive
	}
	live := liveInstance(instances)
	switch {
	case live != nil:
		obs.Node, obs.Addr = live.Node, live.Addr
	case svc.DesiredAddr != "":
		// Without an instance the service is where it would wake: its home
		// and its reserved address, which change when it moves.
		obs.Node, obs.Addr = svc.DesiredNode, svc.DesiredAddr
	}
	switch {
	case !svc.DesiredAwake && live != nil:
		obs.State = stateSleeping
	case !svc.DesiredAwake:
		obs.State = stateAsleep
	case live != nil:
		obs.State = stateActive
	case failed:
		obs.State = stateFailed
	case svc.State == stateCreating:
		obs.State = stateCreating
	default:
		obs.State = stateWaking
	}
	return obs, false
}
