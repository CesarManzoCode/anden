package agent

import (
	andenv1 "github.com/CesarManzoCode/anden/gen/anden/v1"
)

// local is what diff needs to know about an instance the agent has.
type local struct {
	service  string
	stopping bool
}

// diff compares what the control plane asks for with what the node has and
// returns what to start and which instances to stop. An instance that is not
// asked for, or whose service must sleep, stops; a failed one stays, for
// inspection, until its generation changes. A new instance of a service waits
// until the previous one is gone, because both would use the same address.
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
