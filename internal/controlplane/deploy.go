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

// Phases of a rollout (D24), stored in deployments.phase. In starting the new
// instance runs beside the old one until its healthcheck passes; the switch of
// the service row moves the edge and the DNS to it and begins overlap, where
// the old one keeps running; in draining the old one is no longer assigned,
// and its agent sends SIGTERM and, draining_seconds later, SIGKILL.
const (
	phaseStarting = "starting"
	phaseOverlap  = "overlap"
	phaseDraining = "draining"
)

// drainLimit is how long after draining_seconds a deployment waits for the old
// instance to be reported gone. Past it the deployment ends anyway: the node
// may be dead and never report again.
const drainLimit = 10 * time.Second

// rolloutWanted says whether a deploy of svc starts its new instance beside the
// running one. Otherwise the new spec replaces the old one at once: a service
// that sleeps has no traffic to protect, and is deployed when it wakes (D24); a
// singleton never has two instances, so its old one stops before the new one
// starts.
func rolloutWanted(svc store.Service) bool {
	if svc.Deleted || !svc.DesiredAwake || svc.State != stateActive || svc.DesiredNode == "" || svc.DesiredAddr == "" {
		return false
	}
	spec, err := specOf(svc)
	return err == nil && !spec.GetSingleton()
}

type deployStep int

const (
	stepWait deployStep = iota
	stepSwitch
	stepFail
	stepDrain
	stepEnd
)

// nextDeployStep is what a deployment does now given its service, whether
// that service still exists, and the instances reported for it.
func nextDeployStep(d store.Deployment, spec *andenv1.ServiceSpec, svc store.Service, found bool, instances []store.Instance, now time.Time) deployStep {
	elapsed := now.Sub(d.Since)
	seconds := func(v uint32) time.Duration { return time.Duration(v) * time.Second }
	switch d.Phase {
	case phaseStarting:
		// A delete, a move or a new generation of the service gives it up.
		if !found || svc.Deleted || svc.Generation != d.OldGeneration {
			return stepEnd
		}
		for _, in := range instances {
			if in.Service != d.Service || in.Generation != d.Generation {
				continue
			}
			switch in.State {
			case instRunning:
				return stepSwitch
			case instFailed:
				return stepFail
			}
		}
		if elapsed >= seconds(spec.GetHealthcheckTimeoutSeconds()) {
			return stepFail
		}
	case phaseOverlap:
		if elapsed >= seconds(spec.GetOverlapSeconds()) {
			return stepDrain
		}
	case phaseDraining:
		if !found || elapsed >= seconds(spec.GetDrainingSeconds())+drainLimit {
			return stepEnd
		}
		if elapsed < seconds(spec.GetDrainingSeconds()) {
			return stepWait
		}
		for _, in := range instances {
			if in.Service == d.Service && in.Generation == d.OldGeneration {
				return stepWait
			}
		}
		return stepEnd
	default:
		return stepEnd
	}
	return stepWait
}

// rollout carries each deployment one step further and reports whether any
// changed. Everything it reads and writes is in the store, with the time each
// phase began, so a new andend resumes a deployment where the last one left it.
func (r *Reconciler) rollout(ctx context.Context, services []store.Service, instances []store.Instance, deploys []store.Deployment) (bool, error) {
	byName := make(map[string]store.Service, len(services))
	for _, svc := range services {
		byName[svc.Name] = svc
	}
	now := r.now()
	changed := false
	for _, d := range deploys {
		spec := &andenv1.ServiceSpec{}
		if err := proto.Unmarshal(d.Spec, spec); err != nil {
			return changed, fmt.Errorf("stored spec of the deploy of %s: %w", d.Service, err)
		}
		svc, found := byName[d.Service]
		var err error
		switch nextDeployStep(d, spec, svc, found, instances, now) {
		case stepWait:
			continue
		case stepSwitch:
			// An old instance that already sleeps has nothing to overlap.
			next := phaseOverlap
			if !svc.DesiredAwake {
				next = phaseDraining
			}
			var ok bool
			if ok, err = r.state.SwitchDeploy(ctx, d.Service, d.Generation, phaseStarting, next, now); ok {
				log.Printf("deploy %s: v%d passed its healthcheck at %s, edge and dns switched", d.Service, d.Version, d.Addr)
			}
		case stepFail:
			log.Printf("deploy %s: v%d did not pass its healthcheck, v%d stays active", d.Service, d.Version, d.OldVersion)
			err = r.state.EndDeploy(ctx, d.Service, d.Generation)
		case stepDrain:
			_, err = r.state.SetDeployPhase(ctx, d.Service, d.Generation, phaseOverlap, phaseDraining, now)
		case stepEnd:
			err = r.state.EndDeploy(ctx, d.Service, d.Generation)
		}
		if err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

// deployAssignments adds to what a node receives for its services the
// instances of the deployments under way there: the new one while it starts,
// and the old one during overlap. While a deployment has not reached draining
// its old instance carries the draining_seconds of the deploy, which the agent
// waits between SIGTERM and SIGKILL when it is no longer assigned, and an idle
// window of 0, so that it does not sleep in the middle of the deploy.
func deployAssignments(node string, assignments []*andenv1.Assignment, deploys []store.Deployment) ([]*andenv1.Assignment, error) {
	byID := make(map[string]*andenv1.Assignment, len(assignments))
	for _, as := range assignments {
		byID[as.GetInstanceId()] = as
	}
	out := assignments
	for _, d := range deploys {
		spec := &andenv1.ServiceSpec{}
		if err := proto.Unmarshal(d.Spec, spec); err != nil {
			return nil, fmt.Errorf("stored spec of the deploy of %s: %w", d.Service, err)
		}
		if d.Phase == phaseStarting && d.Node == node && d.Addr != "" {
			id := InstanceID(d.Service, d.Generation)
			if byID[id] == nil {
				fresh := proto.CloneOf(spec)
				fresh.DrainingSeconds = nil
				as := &andenv1.Assignment{InstanceId: id, Spec: fresh, Version: uint64(d.Version),
					Generation: uint64(d.Generation), Addr: d.Addr, Awake: true}
				byID[id] = as
				out = append(out, as)
			}
		}
		if d.OldNode != node || (d.Phase != phaseStarting && d.Phase != phaseOverlap) {
			continue
		}
		id := InstanceID(d.Service, d.OldGeneration)
		as := byID[id]
		if as == nil {
			if d.Phase != phaseOverlap {
				continue
			}
			old := &andenv1.ServiceSpec{}
			if err := proto.Unmarshal(d.OldSpec, old); err != nil {
				return nil, fmt.Errorf("stored old spec of the deploy of %s: %w", d.Service, err)
			}
			as = &andenv1.Assignment{InstanceId: id, Spec: old, Version: uint64(d.OldVersion),
				Generation: uint64(d.OldGeneration), Addr: d.OldAddr, Awake: true}
			byID[id] = as
			out = append(out, as)
		}
		as.Spec = proto.CloneOf(as.GetSpec())
		as.Spec.DrainingSeconds = proto.Uint32(spec.GetDrainingSeconds())
		as.Spec.IdleSeconds = proto.Uint32(0)
	}
	return out, nil
}
