// Package wake is the wake proxy of D04 and the sleep of a service: the state
// machine, the sleeping set in nftables, the proxy that holds connections on
// :15000 and the idle signal of D05. It knows nothing about instances: the
// agent gives each Machine the Actions that stop and start one.
package wake

import (
	"errors"
	"fmt"
	"log"
	"net/netip"
	"sync"
	"time"
)

// State of a service on its node. The cycle is Active, Sleeping, Asleep, Waking
// and Active again; Failed and Closed end it.
type State int

const (
	Active State = iota
	Sleeping
	Asleep
	Waking
	Failed
	Closed
)

func (s State) String() string {
	switch s {
	case Active:
		return "ACTIVE"
	case Sleeping:
		return "SLEEPING"
	case Asleep:
		return "ASLEEP"
	case Waking:
		return "WAKING"
	case Failed:
		return "FAILED"
	case Closed:
		return "CLOSED"
	}
	return fmt.Sprintf("State(%d)", int(s))
}

const (
	// settleDelay lets a SYN that passed nftables before the target entered
	// the set reach the service, and so show up when its sockets are read.
	settleDelay   = time.Millisecond
	probeInterval = 2 * time.Millisecond
	probeTimeout  = 10 * time.Second
)

var (
	// ErrBusy aborts a sleep: something still uses the service.
	ErrBusy = errors.New("the service has live connections")
	// ErrNotActive refuses a sleep from any state but Active.
	ErrNotActive = errors.New("the service is not active")
	// ErrUnavailable tells a held connection that the service will not wake.
	ErrUnavailable = errors.New("the service cannot be woken")
	// ErrDead lets Probe end the wake early: no process is left to listen.
	ErrDead = errors.New("the process of the service exited")
)

// Actions are what a Machine asks of the node. Only one of Halt and Start runs
// at a time, and the Machine holds no lock while calling any of them.
type Actions interface {
	// Hold adds target to the sleeping set and Release takes it out.
	Hold(target netip.AddrPort) error
	Release(target netip.AddrPort) error
	// LiveSockets counts the sockets of the service that stop it from sleeping.
	LiveSockets() (int, error)
	// Halt ends the processes and frees what they occupied. Once it is called
	// the sleep is decided.
	Halt() error
	// Start brings the processes back.
	Start() error
	// Probe makes one attempt to connect to the service.
	Probe() error
	// Ready is called when the service answers and is out of the set.
	Ready()
	// Fail is called when the machine gives up on the service.
	Fail(err error)
}

// Machine is the state of one service. Every path to sleep or to wake goes
// through it, so a service wakes once however many connections arrive.
type Machine struct {
	target netip.AddrPort
	act    Actions

	probeTimeout time.Duration
	closing      chan struct{}
	closeOnce    sync.Once

	mu    sync.Mutex
	state State
	// held says the target is in the sleeping set.
	held bool
	// pending is a wake asked for during Sleeping.
	pending bool
	// conns counts connections the proxy is serving. A connection still dialing
	// has no socket in the service yet, so reading its sockets would miss it.
	conns int
	// changed is closed and replaced at each change of state.
	changed chan struct{}
}

func newMachine(target netip.AddrPort, act Actions, state State) *Machine {
	return &Machine{
		target:       target,
		act:          act,
		probeTimeout: probeTimeout,
		closing:      make(chan struct{}),
		state:        state,
		changed:      make(chan struct{}),
	}
}

// NewMachine returns the machine of a service that is, or is about to be, up.
func NewMachine(target netip.AddrPort, act Actions) *Machine {
	return newMachine(target, act, Active)
}

// NewDormant returns the machine of a service assigned to this node asleep,
// with no processes. It is Sleeping until Settle puts its target in the set.
func NewDormant(target netip.AddrPort, act Actions) *Machine {
	return newMachine(target, act, Sleeping)
}

// Target is the address and port of the service.
func (m *Machine) Target() netip.AddrPort { return m.target }

// State returns the current state.
func (m *Machine) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// setLocked changes the state and wakes whoever waits for a change.
func (m *Machine) setLocked(state State) {
	m.state = state
	close(m.changed)
	m.changed = make(chan struct{})
}

// Sleep takes the service to Asleep in the order that loses no connection: from
// the moment the target is in the set every new connection is held by the proxy,
// so once a later read of the sockets finds nothing, nothing can arrive at the
// service. A live socket takes the target out of the set again and returns
// ErrBusy with the service untouched; what the proxy held meanwhile is served.
func (m *Machine) Sleep() error {
	m.mu.Lock()
	if m.state != Active {
		m.mu.Unlock()
		return ErrNotActive
	}
	m.setLocked(Sleeping)
	m.mu.Unlock()

	if err := m.act.Hold(m.target); err != nil {
		m.abort()
		return err
	}
	m.mu.Lock()
	m.held = true
	m.mu.Unlock()

	time.Sleep(settleDelay)
	live, err := m.act.LiveSockets()
	m.mu.Lock()
	conns := m.conns
	m.mu.Unlock()
	if err == nil && (live > 0 || conns > 0) {
		err = ErrBusy
	}
	if err != nil {
		if rerr := m.act.Release(m.target); rerr != nil {
			err = errors.Join(err, rerr)
		} else {
			m.mu.Lock()
			m.held = false
			m.mu.Unlock()
		}
		m.abort()
		return err
	}

	if err := m.act.Halt(); err != nil {
		m.fail(err)
		return err
	}
	m.settle()
	return nil
}

// abort returns to Active. The connections waiting in Serve go on to the service.
func (m *Machine) abort() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending = false
	m.setLocked(Active)
}

// Settle finishes entering the sleep of a machine from NewDormant.
func (m *Machine) Settle() error {
	if err := m.act.Hold(m.target); err != nil {
		m.fail(err)
		return err
	}
	m.mu.Lock()
	m.held = true
	m.mu.Unlock()
	m.settle()
	return nil
}

// settle ends Sleeping: Asleep or, if a wake came meanwhile, straight to Waking.
func (m *Machine) settle() {
	m.mu.Lock()
	defer m.mu.Unlock()
	pending := m.pending
	m.pending = false
	select {
	case <-m.closing:
		pending = false
	default:
	}
	if pending {
		m.beginWakeLocked()
		return
	}
	m.setLocked(Asleep)
}

// Wake asks for the service to wake. It is safe to call from anywhere and any
// number of times: only the change out of Asleep starts anything.
func (m *Machine) Wake() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.wakeLocked()
}

func (m *Machine) wakeLocked() {
	switch m.state {
	case Asleep:
		m.beginWakeLocked()
	case Sleeping:
		m.pending = true
	}
}

func (m *Machine) beginWakeLocked() {
	m.setLocked(Waking)
	go m.runWake()
}

// runWake starts the service, waits until it accepts connections and only then
// takes the target out of the set, so new connections never find it closed.
func (m *Machine) runWake() {
	err := m.act.Start()
	if err == nil {
		err = m.awaitListening()
	}
	if err == nil {
		err = m.act.Release(m.target)
	}
	if err != nil {
		m.fail(err)
		return
	}
	m.act.Ready()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.held = false
	m.setLocked(Active)
}

// awaitListening polls the service until it answers or the time is up.
func (m *Machine) awaitListening() error {
	deadline := time.Now().Add(m.probeTimeout)
	for {
		err := m.act.Probe()
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrDead) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-m.closing:
			return err
		case <-time.After(probeInterval):
		}
	}
}

// fail gives up: held connections are closed and the target stays in the set,
// so that later connections are closed too instead of finding nothing.
func (m *Machine) fail(err error) {
	m.act.Fail(err)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending = false
	m.setLocked(Failed)
}

// Serve holds a connection until the service is Active, waking it if needed.
// The caller may then connect, and calls the returned function when it is done.
func (m *Machine) Serve(done <-chan struct{}) (func(), error) {
	for {
		m.mu.Lock()
		switch m.state {
		case Active:
			m.conns++
			m.mu.Unlock()
			return m.releaseConn, nil
		case Failed, Closed:
			m.mu.Unlock()
			return nil, ErrUnavailable
		}
		m.wakeLocked()
		changed := m.changed
		m.mu.Unlock()
		select {
		case <-changed:
		case <-done:
			return nil, ErrUnavailable
		}
	}
}

func (m *Machine) releaseConn() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.conns--
}

// Close ends the machine for good: it waits for a transition in progress, closes
// held connections and takes the target out of the set.
func (m *Machine) Close() {
	m.closeOnce.Do(func() { close(m.closing) })
	m.mu.Lock()
	for m.state == Sleeping || m.state == Waking {
		changed := m.changed
		m.mu.Unlock()
		<-changed
		m.mu.Lock()
	}
	m.setLocked(Closed)
	held := m.held
	m.held = false
	m.mu.Unlock()
	if held {
		if err := m.act.Release(m.target); err != nil {
			log.Printf("wake %s: %v", m.target, err)
		}
	}
}
