// Package controlplane is andend: the gRPC API, the AgentControl service for
// node agents and the level reconciler that compares desired state with what
// agents report (contract D02 and D23).
package controlplane

import (
	"context"
	"log"
	"time"

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
	SetObserved(ctx context.Context, name string, revision int64, obs store.Observed) (bool, error)
	Purge(ctx context.Context, name string, revision int64) error
}

// Reconciler is level based: each pass reads the whole desired and reported
// state and writes the observed state that follows from it. For now it only
// persists; acting on agents comes with them.
type Reconciler struct {
	state State
	kick  chan struct{}
}

func NewReconciler(state State) *Reconciler {
	return &Reconciler{state: state, kick: make(chan struct{}, 1)}
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
	return nil
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
			failed = failed || in.Version == svc.Version
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
