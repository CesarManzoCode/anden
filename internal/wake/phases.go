package wake

import (
	"encoding/json"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Phase is one part of a wake, in the order a wake goes through them.
type Phase int

const (
	// Detection runs from the accept of the connection to the start of the wake.
	Detection Phase = iota
	// Placement waits for the control plane to answer the wake.
	Placement
	Rootfs
	Network
	// Start is the launch or the restore of the processes.
	Start
	// Readiness runs until the service accepts connections and is Active.
	Readiness
	// FirstByte runs until the first byte the service sends reaches the client.
	FirstByte
	phaseCount
)

var phaseNames = [phaseCount]string{"detection", "placement", "rootfs", "network", "start", "readiness", "first_byte"}

func (p Phase) String() string {
	if p < 0 || p >= phaseCount {
		return "unknown"
	}
	return phaseNames[p]
}

// Phases is the breakdown of one wake on the monotonic clock. Each mark closes a
// phase with the time since the previous mark, and a phase marked again adds to
// what it had, so the phases always add up to the time from the accept to the
// last mark. A nil *Phases takes marks and keeps nothing.
type Phases struct {
	// Client is the address of the connection that woke the service, empty
	// when the wake came from elsewhere.
	Client string

	mu        sync.Mutex
	accept    time.Time
	acceptNS  int64
	last      time.Time
	durations [phaseCount]time.Duration
	// woke says this connection started a wake.
	woke bool
}

// NewPhases starts a breakdown now, as the accept of a connection from client.
func NewPhases(client string) *Phases {
	now := time.Now()
	return &Phases{Client: client, accept: now, acceptNS: monotonicNS(), last: now}
}

func (p *Phases) setWoke() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.woke = true
}

// Woke reports whether the connection of this breakdown started a wake. A nil
// *Phases started none.
func (p *Phases) Woke() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.woke
}

// Mark closes phase now.
func (p *Phases) Mark(phase Phase) {
	if p == nil {
		return
	}
	p.mark(phase, time.Now())
}

func (p *Phases) mark(phase Phase, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d := now.Sub(p.last); d > 0 {
		p.durations[phase] += d
	}
	if now.After(p.last) {
		p.last = now
	}
}

// Total is the time from the accept to the last mark.
func (p *Phases) Total() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last.Sub(p.accept)
}

// Millis returns each phase in milliseconds.
func (p *Phases) Millis() PhasesMS {
	p.mu.Lock()
	defer p.mu.Unlock()
	ms := func(ph Phase) float64 { return float64(p.durations[ph]) / float64(time.Millisecond) }
	return PhasesMS{
		Detection: ms(Detection), Placement: ms(Placement), Rootfs: ms(Rootfs), Network: ms(Network),
		Start: ms(Start), Readiness: ms(Readiness), FirstByte: ms(FirstByte),
	}
}

// PhasesMS is phases_ms of a wake event (contract, section 5).
type PhasesMS struct {
	Detection float64 `json:"detection"`
	Placement float64 `json:"placement"`
	Rootfs    float64 `json:"rootfs"`
	Network   float64 `json:"network"`
	Start     float64 `json:"start"`
	Readiness float64 `json:"readiness"`
	FirstByte float64 `json:"first_byte"`
}

// Event is one line of wake_events.jsonl (contract, section 5). TAcceptNS is
// CLOCK_MONOTONIC at the accept, the clock Python reads with time.monotonic_ns.
type Event struct {
	Service        string   `json:"service"`
	Route          string   `json:"route"`
	RouteRequested string   `json:"route_requested"`
	ClientAddr     string   `json:"client_addr"`
	TAcceptNS      int64    `json:"t_accept_ns"`
	PhasesMS       PhasesMS `json:"phases_ms"`
}

// Event returns the line of this wake for a service.
func (p *Phases) Event(service, route, requested string) Event {
	p.mu.Lock()
	acceptNS := p.acceptNS
	p.mu.Unlock()
	return Event{Service: service, Route: route, RouteRequested: requested, ClientAddr: p.Client, TAcceptNS: acceptNS,
		PhasesMS: p.Millis()}
}

// Line is the event as one line of JSON, newline included.
func (e Event) Line() ([]byte, error) {
	raw, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// monotonicNS reads CLOCK_MONOTONIC. time.Now keeps a monotonic reading but
// does not expose its value.
func monotonicNS() int64 {
	var ts syscall.Timespec
	const clockMonotonic = 1
	if _, _, e := syscall.RawSyscall(syscall.SYS_CLOCK_GETTIME, clockMonotonic, uintptr(unsafe.Pointer(&ts)), 0); e != 0 {
		return 0
	}
	return ts.Nano()
}
