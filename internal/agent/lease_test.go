package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	andenv1 "github.com/CesarManzoCode/anden/gen/anden/v1"
	"github.com/CesarManzoCode/anden/internal/wake"
)

// fakeClock is a monotonic clock that only moves when told to.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time             { return c.t }
func (c *fakeClock) advance(d time.Duration)    { c.t = c.t.Add(d) }
func newFakeClock() *fakeClock                  { return &fakeClock{t: time.Unix(1000, 0)} }
func (c *fakeClock) lease() (*lease, time.Time) { return newLease(c.now), c.t }

func TestLeaseIsNotHeldBeforeRegistering(t *testing.T) {
	c := newFakeClock()
	l, _ := c.lease()
	if l.held() || l.valid() || l.expire() {
		t.Fatal("a lease never granted is neither held, valid nor expiring")
	}
}

func TestLeaseExpiresAfterTTL(t *testing.T) {
	c := newFakeClock()
	l, t0 := c.lease()
	l.grant(t0, 2*time.Second)
	c.advance(2*time.Second - time.Nanosecond)
	if !l.valid() || l.expire() {
		t.Fatal("valid until the TTL")
	}
	c.advance(time.Nanosecond)
	if l.valid() {
		t.Fatal("still valid at the TTL")
	}
	if !l.expire() {
		t.Fatal("expire must report the end of the lease")
	}
	if l.expire() || l.held() {
		t.Fatal("expire reports once and drops the lease")
	}
}

func TestLeaseRenewsFromWhenTheHeartbeatWasSent(t *testing.T) {
	c := newFakeClock()
	l, t0 := c.lease()
	l.grant(t0, 2*time.Second)
	c.advance(1500 * time.Millisecond)
	sent := c.now()
	// The answer arrives 300 ms later: the lease counts from the request.
	c.advance(300 * time.Millisecond)
	l.renew(sent, 0)
	c.advance(1699 * time.Millisecond)
	if !l.valid() {
		t.Fatal("renewed lease ended early")
	}
	c.advance(time.Millisecond)
	if l.valid() {
		t.Fatal("the lease must end 2 s after the heartbeat was sent, not after its answer")
	}
	// An older send does not move the lease back.
	l2, t1 := c.lease()
	l2.grant(t1, 2*time.Second)
	l2.renew(t1.Add(-time.Second), 0)
	c.advance(1999 * time.Millisecond)
	if !l2.valid() {
		t.Fatal("an older renewal shortened the lease")
	}
}

func TestLeaseTakesTheTTLOfTheControlPlane(t *testing.T) {
	c := newFakeClock()
	l, t0 := c.lease()
	l.grant(t0, 0)
	c.advance(defaultLeaseTTL - time.Millisecond)
	if !l.valid() {
		t.Fatal("a TTL of 0 keeps the default")
	}
	l.renew(c.now(), time.Second)
	c.advance(time.Second)
	if l.valid() {
		t.Fatal("the TTL of the heartbeat answer was not taken")
	}
}

func TestLeaseOnlyComesBackWithARegistration(t *testing.T) {
	c := newFakeClock()
	l, t0 := c.lease()
	l.grant(t0, 2*time.Second)
	c.advance(2 * time.Second)
	if !l.expire() {
		t.Fatal("not expired")
	}
	// A heartbeat sent before the fence answers late.
	l.renew(c.now(), 0)
	if l.held() || l.valid() {
		t.Fatal("a heartbeat revived a fenced lease")
	}
	l.grant(c.now(), 0)
	if !l.valid() {
		t.Fatal("a new registration must hold the lease again")
	}
}

func expiredAgent(c *fakeClock) *Agent {
	l, t0 := c.lease()
	l.grant(t0, 2*time.Second)
	c.advance(2 * time.Second)
	return &Agent{lease: l, instances: map[string]*instance{}, intents: map[string]*intent{}, beatNow: make(chan struct{}, 1)}
}

func TestNoWakeWithoutLease(t *testing.T) {
	a := expiredAgent(newFakeClock())
	in := &instance{id: "a-1", service: "a", generation: 1, state: andenv1.InstanceState_INSTANCE_STATE_STOPPED}
	err := (&instanceControl{a: a, in: in}).Start(wake.NewPhases(""))
	if !errors.Is(err, errFenced) {
		t.Fatalf("Start without a lease: %v", err)
	}
	if in.state != andenv1.InstanceState_INSTANCE_STATE_STOPPED || len(a.intents) != 0 {
		t.Fatalf("a refused wake changed the instance (%v) or declared an intent (%v)", in.state, a.intents)
	}
}

func TestNoStartWithoutLease(t *testing.T) {
	a := expiredAgent(newFakeClock())
	a.desired = []*andenv1.Assignment{assignment("a", 1, true), assignment("b", 1, false)}
	a.reconcile(context.Background())
	if len(a.instances) != 0 {
		t.Fatalf("reconcile started %d instances without a lease", len(a.instances))
	}
}

func TestKillInstancesKillsEveryInstanceCgroup(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"agent", "svc-a-1", "svc-b-2"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "svc-file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := killInstances(dir); got != 2 {
		t.Fatalf("killed %d cgroups, want 2", got)
	}
	for _, d := range []string{"svc-a-1", "svc-b-2"} {
		raw, err := os.ReadFile(filepath.Join(dir, d, "cgroup.kill"))
		if err != nil || string(raw) != "1" {
			t.Errorf("%s/cgroup.kill: %q %v", d, raw, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "agent", "cgroup.kill")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the agent's own cgroup must survive the fence")
	}
}
