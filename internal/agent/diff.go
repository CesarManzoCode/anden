package agent

import (
	andenv1 "github.com/CesarManzoCode/anden/gen/anden/v1"
	"github.com/CesarManzoCode/anden/internal/wake"
)

// local is what diff needs to know about an instance the agent has.
type local struct {
	service  string
	stopping bool
}

// diff compares what the control plane asks for with what the node has and
// returns what to start and which instances must not stay as they are. An
// instance that is not asked for, or whose service must sleep, is among those
// (sleepAction tells them apart); a failed one stays, for inspection, until its
// generation changes. A new instance of a service waits until the previous one
// is gone, because both would use the same address.
func diff(desired []*andenv1.Assignment, have map[string]local) (start []*andenv1.Assignment, stop []string) {
	awake := make(map[string]bool, len(desired))
	for _, as := range desired {
		if as.GetAwake() {
			awake[as.GetInstanceId()] = true
		}
	}
	busy := make(map[string]bool, len(have))
	for id, in := range have {
		busy[in.service] = true
		if !awake[id] && !in.stopping {
			stop = append(stop, id)
		}
	}
	for _, as := range desired {
		if _, ok := have[as.GetInstanceId()]; as.GetAwake() && !ok && !busy[as.GetSpec().GetName()] {
			start = append(start, as)
		}
	}
	return start, stop
}

// dormant returns the services assigned asleep of which the node has nothing:
// after a restart of the agent, or a deploy that changed the generation of a
// sleeping service. Each needs its place in the sleeping set to be woken.
func dormant(desired []*andenv1.Assignment, have map[string]local) []*andenv1.Assignment {
	busy := make(map[string]bool, len(have))
	for _, in := range have {
		busy[in.service] = true
	}
	var out []*andenv1.Assignment
	for _, as := range desired {
		if !as.GetAwake() && !busy[as.GetSpec().GetName()] {
			out = append(out, as)
		}
	}
	return out
}

type action int

const (
	actionNone action = iota
	actionSleep
	actionStop
)

// sleepAction says what to do with an instance among those diff wants gone. One
// that is not assigned stops. One that must sleep goes through the machine if it
// is running; if a transition is under way, the next pass looks again; if it never
// got to run, or died, it stops and dormant replaces it.
func sleepAction(mustSleep bool, machine wake.State, state andenv1.InstanceState) action {
	if !mustSleep {
		return actionStop
	}
	switch machine {
	case wake.Sleeping, wake.Asleep, wake.Waking:
		return actionNone
	case wake.Active:
		if state == andenv1.InstanceState_INSTANCE_STATE_RUNNING {
			return actionSleep
		}
	}
	return actionStop
}
