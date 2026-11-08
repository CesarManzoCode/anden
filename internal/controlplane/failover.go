package controlplane

import (
	"context"
	"log"

	"github.com/CesarManzoCode/anden/internal/scheduler"
	"github.com/CesarManzoCode/anden/internal/store"
)

// liveOnly leaves out what dead nodes reported: their instances were fenced
// or died with the node, and their rows stay until the node registers again.
func liveOnly(instances []store.Instance, ready func(string) bool) []store.Instance {
	out := make([]store.Instance, 0, len(instances))
	for _, in := range instances {
		if ready(in.Node) {
			out = append(out, in)
		}
	}
	return out
}

// relocate takes the services of dead nodes off them (D08). A node is dead
// once it was not heard from in 2×TTL, a TTL after its agent fenced itself,
// so nothing moves while the old instance can still run. An awake service
// loses its node and address, and place gives it new ones in the same pass
// with the scheduler, over the ready nodes and in one batch in decreasing
// order of size. A sleeping one does not start: the scheduler picks its new
// home, on top of what the awake services reserve there, and place gives it
// an address of that node; its snapshot stays behind, so it wakes by relaunch.
// Both go to a new generation. What fits nowhere waits for a later pass.
func (r *Reconciler) relocate(ctx context.Context, services []store.Service, nodes []store.Node) error {
	dead := map[string]bool{}
	problem := scheduler.Problem{}
	for _, n := range nodes {
		if r.ready(n.Name) {
			problem.Nodes = append(problem.Nodes, scheduler.Node{Name: n.Name, CPUMillis: int64(n.CPUMillis), MemMiB: int64(n.MemoryMiB)})
		} else {
			dead[n.Name] = true
		}
	}
	if len(dead) == 0 {
		return nil
	}
	var asleep []int
	for i, svc := range services {
		if svc.Deleted || svc.DesiredNode == "" {
			continue
		}
		spec, err := specOf(svc)
		if err != nil {
			return err
		}
		want := scheduler.Service{
			Name:              svc.Name,
			CPUMillis:         int64(spec.GetCpuMillis()),
			MemMiB:            int64(spec.GetMemoryMib()),
			AntiAffinityGroup: spec.GetAntiAffinityGroup(),
		}
		switch {
		case !dead[svc.DesiredNode]:
			if svc.DesiredAwake {
				want.Node = svc.DesiredNode
				problem.Services = append(problem.Services, want)
			}
		case svc.DesiredAwake:
			if err := r.moveOff(ctx, &services[i], ""); err != nil {
				return err
			}
		default:
			asleep = append(asleep, i)
			problem.Services = append(problem.Services, want)
		}
	}
	if len(asleep) == 0 {
		return nil
	}
	placement := scheduler.Solve(problem)
	for _, i := range asleep {
		if node := placement[services[i].Name]; node != "" {
			if err := r.moveOff(ctx, &services[i], node); err != nil {
				return err
			}
		}
	}
	return nil
}

// moveOff takes svc off its dead node to node to, or to none, and records the
// change in svc. A lost revision race leaves it for the next pass.
func (r *Reconciler) moveOff(ctx context.Context, svc *store.Service, to string) error {
	from := svc.DesiredNode
	ok, err := r.state.Relocate(ctx, svc.Name, svc.Revision, from, to)
	if err != nil || !ok {
		return err
	}
	svc.DesiredNode, svc.DesiredAddr = to, ""
	svc.Generation++
	svc.Revision++
	log.Printf("node %s is dead: %s leaves it in generation %d (awake %v)", from, svc.Name, svc.Generation, svc.DesiredAwake)
	return nil
}
