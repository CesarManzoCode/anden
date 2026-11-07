package agent

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	andenv1 "github.com/CesarManzoCode/anden/gen/anden/v1"
)

const (
	// defaultLeaseTTL is the TTL of D08 until the control plane gives one.
	defaultLeaseTTL = 2 * time.Second
	// leaseCheck is how often the agent looks at its lease: the fence comes at
	// most this long after the TTL.
	leaseCheck = 50 * time.Millisecond
)

// errFenced refuses a wake while the node has no lease.
var errFenced = errors.New("the lease expired: the node is fenced until it registers again")

// lease is the agent's side of D08, on the monotonic clock. A renewal counts
// from the moment its request was sent, not from its answer: the control plane
// heard the agent after that moment, so the lease always ends before the
// control plane counts 2×TTL. Once it ends only a new registration holds it
// again; a late answer to a heartbeat sent before does not.
type lease struct {
	mu      sync.Mutex
	now     func() time.Time
	ttl     time.Duration
	renewed time.Time
	active  bool
}

func newLease(now func() time.Time) *lease {
	return &lease{now: now, ttl: defaultLeaseTTL}
}

// grant holds the lease from sent, when a registration that gave a new epoch
// was sent. A ttl of 0 keeps the last one.
func (l *lease) grant(sent time.Time, ttl time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ttl > 0 {
		l.ttl = ttl
	}
	l.renewed, l.active = sent, true
}

// renew extends a lease still held to sent, when an accepted heartbeat was sent.
func (l *lease) renew(sent time.Time, ttl time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.active {
		return
	}
	if ttl > 0 {
		l.ttl = ttl
	}
	if sent.After(l.renewed) {
		l.renewed = sent
	}
}

// held says the lease was granted and has not been dropped by expire.
func (l *lease) held() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active
}

// valid says the lease is held and within its TTL now.
func (l *lease) valid() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active && l.now().Sub(l.renewed) < l.ttl
}

// expire drops a held lease whose TTL ran out and reports it, once.
func (l *lease) expire() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.active || l.now().Sub(l.renewed) < l.ttl {
		return false
	}
	l.active = false
	return true
}

func msDuration(ms uint32) time.Duration { return time.Duration(ms) * time.Millisecond }

// fenceLoop fences the node as soon as its lease runs out, whatever the
// heartbeat is doing meanwhile.
func (a *Agent) fenceLoop(ctx context.Context) {
	tick := time.NewTicker(leaseCheck)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if a.lease.expire() {
			a.fence()
		}
	}
}

// fence is the self fencing of D08. It kills every instance of the node at
// once with cgroup.kill, forgets what the control plane asked, and then ends
// each instance for good: its machine closes the connections it holds, and
// its network, rootfs and cgroup go. Nothing wakes until a registration with
// a new epoch, after which the node runs only what the control plane still
// gives it.
func (a *Agent) fence() {
	a.mu.Lock()
	a.epoch = 0
	a.desired = nil
	clear(a.intents)
	victims := make([]*instance, 0, len(a.instances))
	for _, in := range a.instances {
		in.cancel()
		if in.stopping {
			continue
		}
		in.stopping = true
		// Not reported again: the processes are gone.
		in.state = andenv1.InstanceState_INSTANCE_STATE_STOPPED
		victims = append(victims, in)
	}
	a.changes++
	a.mu.Unlock()
	killed := killInstances(a.cfg.Cgroup)
	log.Printf("lease expired: node fenced, cgroup.kill on %d instance cgroups", killed)
	for _, in := range victims {
		go a.remove(in, 0)
	}
}

// killInstances writes cgroup.kill in every instance cgroup of the node, those
// the agent knows and any other svc-* left there, and returns how many.
func killInstances(nodeCgroup string) int {
	entries, err := os.ReadDir(nodeCgroup)
	if err != nil {
		log.Printf("fence: %v", err)
		return 0
	}
	killed := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "svc-") {
			continue
		}
		if err := killCgroup(filepath.Join(nodeCgroup, e.Name())); err != nil {
			log.Printf("fence: %v", err)
			continue
		}
		killed++
	}
	return killed
}
