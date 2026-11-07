// Package controlplane is andend: the gRPC API, the AgentControl service for
// node agents, the blob server, the edge and the level reconciler that compares
// desired state with what agents report (contract D02, D21 and D23).
package controlplane

import (
	"context"
	"fmt"
	"log"
	"time"

	"google.golang.org/protobuf/proto"

	andenv1 "github.com/CesarManzoCode/anden/gen/anden/v1"
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
}

func NewReconciler(state State) *Reconciler {
	return &Reconciler{state: state, kick: make(chan struct{}, 1), ready: func(string) bool { return true }}
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
	if err := r.place(ctx, services); err != nil {
		return err
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

// place gives a node and an address to every awake service without an address
// and records them in services. A service that fits nowhere waits for a later
// pass; lost revision races too.
func (r *Reconciler) place(ctx context.Context, services []store.Service) error {
	nodes, err := r.state.ListNodes(ctx)
	if err != nil {
		return err
	}
	subnets := map[string]string{}
	for _, n := range nodes {
		subnets[n.Name] = n.Subnet
	}
	reserved := map[string]resources{}
	used := map[string]bool{}
	needs := make([]resources, len(services))
	for i, svc := range services {
		spec, err := specOf(svc)
		if err != nil {
			return err
		}
		needs[i] = resources{cpuMillis: int64(spec.GetCpuMillis()), memoryMiB: int64(spec.GetMemoryMib())}
		if svc.DesiredAddr != "" {
			used[svc.DesiredAddr] = true
		}
		// D07: only what is awake reserves.
		if svc.DesiredNode != "" && svc.DesiredAwake && !svc.Deleted {
			cur := reserved[svc.DesiredNode]
			reserved[svc.DesiredNode] = resources{cur.cpuMillis + needs[i].cpuMillis, cur.memoryMiB + needs[i].memoryMiB}
		}
	}
	for i := range services {
		svc := &services[i]
		if svc.Deleted || !svc.DesiredAwake || svc.DesiredAddr != "" {
			continue
		}
		node := svc.DesiredNode
		if node == "" {
			node = firstFit(needs[i], nodes, reserved, r.ready)
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
		if svc.DesiredNode == "" {
			cur := reserved[node]
			reserved[node] = resources{cur.cpuMillis + needs[i].cpuMillis, cur.memoryMiB + needs[i].memoryMiB}
		}
		svc.DesiredNode, svc.DesiredAddr = node, addr
		used[addr] = true
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

// InstanceID names the instance of a generation: <service>-<generation>.
func InstanceID(service string, generation int64) string {
	return fmt.Sprintf("%s-%d", service, generation)
}

// toAssignments is what a node's agent receives for the services placed on it.
func toAssignments(services []store.Service) ([]*andenv1.Assignment, error) {
	out := make([]*andenv1.Assignment, 0, len(services))
	for _, svc := range services {
		spec, err := specOf(svc)
		if err != nil {
			return nil, err
		}
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
	if live != nil {
		obs.Node, obs.Addr = live.Node, live.Addr
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
