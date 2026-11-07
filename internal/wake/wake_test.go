package wake

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var testTarget = netip.MustParseAddrPort("10.81.0.2:8080")

// fakeActions records the order of what the machine does.
type fakeActions struct {
	mu     sync.Mutex
	events []string

	live      int
	liveErr   error
	holdErr   error
	haltErr   error
	startErr  error
	probeErr  func(attempt int) error
	attempts  int
	starts    atomic.Int32
	failed    atomic.Int32
	haltGate  chan struct{}
	startGate chan struct{}
}

func (f *fakeActions) note(event string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
}

func (f *fakeActions) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.events)
}

func (f *fakeActions) Hold(netip.AddrPort) error {
	f.note("hold")
	return f.holdErr
}

func (f *fakeActions) Release(netip.AddrPort) error {
	f.note("release")
	return nil
}

func (f *fakeActions) LiveSockets() (int, error) {
	f.note("sockets")
	return f.live, f.liveErr
}

func (f *fakeActions) Halt() error {
	f.note("halt")
	if f.haltGate != nil {
		<-f.haltGate
	}
	return f.haltErr
}

func (f *fakeActions) Start() error {
	f.starts.Add(1)
	f.note("start")
	if f.startGate != nil {
		<-f.startGate
	}
	return f.startErr
}

func (f *fakeActions) Probe() error {
	f.mu.Lock()
	f.attempts++
	attempt := f.attempts
	f.mu.Unlock()
	if f.probeErr != nil {
		if err := f.probeErr(attempt); err != nil {
			return err
		}
	}
	f.note("probe")
	return nil
}

func (f *fakeActions) Ready() { f.note("ready") }

func (f *fakeActions) Fail(error) {
	f.failed.Add(1)
	f.note("fail")
}

// waitState waits for a state the machine reaches on another goroutine.
func waitState(t *testing.T, m *Machine, want State) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for m.State() != want {
		if time.Now().After(deadline) {
			t.Fatalf("state %s, want %s", m.State(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func asleepMachine(t *testing.T, f *fakeActions) *Machine {
	t.Helper()
	m := NewMachine(testTarget, f)
	if err := m.Sleep(); err != nil {
		t.Fatal(err)
	}
	if m.State() != Asleep {
		t.Fatalf("state %s after a sleep", m.State())
	}
	return m
}

func TestSleepOrder(t *testing.T) {
	f := &fakeActions{}
	asleepMachine(t, f)
	want := []string{"hold", "sockets", "halt"}
	if got := f.log(); !slices.Equal(got, want) {
		t.Fatalf("order %q, want %q", got, want)
	}
}

func TestSleepAbortsWithLiveSocket(t *testing.T) {
	f := &fakeActions{live: 1}
	m := NewMachine(testTarget, f)
	if err := m.Sleep(); !errors.Is(err, ErrBusy) {
		t.Fatalf("Sleep = %v, want ErrBusy", err)
	}
	if m.State() != Active {
		t.Fatalf("state %s after an abort", m.State())
	}
	want := []string{"hold", "sockets", "release"}
	if got := f.log(); !slices.Equal(got, want) {
		t.Fatalf("order %q, want %q: the service must not be touched", got, want)
	}
}

func TestSleepAbortsIfSocketsCannotBeRead(t *testing.T) {
	boom := errors.New("boom")
	f := &fakeActions{liveErr: boom}
	m := NewMachine(testTarget, f)
	if err := m.Sleep(); !errors.Is(err, boom) {
		t.Fatalf("Sleep = %v", err)
	}
	if m.State() != Active || slices.Contains(f.log(), "halt") {
		t.Fatalf("state %s, events %q", m.State(), f.log())
	}
}

func TestSleepAbortsWithServedConnection(t *testing.T) {
	f := &fakeActions{}
	m := NewMachine(testTarget, f)
	release, err := m.Serve(nil)
	if err != nil {
		t.Fatal(err)
	}
	// Dialing, it has no socket in the service, but a stop would break it.
	if err := m.Sleep(); !errors.Is(err, ErrBusy) {
		t.Fatalf("Sleep = %v, want ErrBusy", err)
	}
	release()
	if err := m.Sleep(); err != nil {
		t.Fatalf("Sleep after release: %v", err)
	}
}

func TestHeldConnectionSurvivesAbort(t *testing.T) {
	f := &fakeActions{live: 1}
	gate := make(chan struct{})
	act := &gatedSockets{fakeActions: f, gate: gate, entered: make(chan struct{})}
	m := NewMachine(testTarget, act)
	sleepDone := make(chan error, 1)
	go func() { sleepDone <- m.Sleep() }()
	<-act.entered

	served := make(chan error, 1)
	go func() {
		release, err := m.Serve(nil)
		if err == nil {
			release()
		}
		served <- err
	}()
	close(gate)
	if err := <-sleepDone; !errors.Is(err, ErrBusy) {
		t.Fatalf("Sleep = %v", err)
	}
	if err := <-served; err != nil {
		t.Fatalf("held connection: %v", err)
	}
	if f.starts.Load() != 0 {
		t.Fatal("an aborted sleep needs no wake")
	}
}

// gatedSockets stops a sleep where the target is in the set and sockets are read.
type gatedSockets struct {
	*fakeActions
	gate    chan struct{}
	entered chan struct{}
}

func (g *gatedSockets) LiveSockets() (int, error) {
	close(g.entered)
	<-g.gate
	return g.fakeActions.LiveSockets()
}

func TestSleepOnlyFromActive(t *testing.T) {
	f := &fakeActions{}
	m := asleepMachine(t, f)
	if err := m.Sleep(); !errors.Is(err, ErrNotActive) {
		t.Fatalf("Sleep asleep = %v", err)
	}
	f.startGate = make(chan struct{})
	m.Wake()
	if m.State() != Waking {
		t.Fatalf("state %s", m.State())
	}
	if err := m.Sleep(); !errors.Is(err, ErrNotActive) {
		t.Fatalf("Sleep waking = %v", err)
	}
	close(f.startGate)
	waitState(t, m, Active)
}

func TestWakeOrder(t *testing.T) {
	f := &fakeActions{}
	m := asleepMachine(t, f)
	m.Wake()
	waitState(t, m, Active)
	want := []string{"hold", "sockets", "halt", "start", "probe", "release", "ready"}
	if got := f.log(); !slices.Equal(got, want) {
		t.Fatalf("order %q, want %q", got, want)
	}
}

func TestWakeOfActiveDoesNothing(t *testing.T) {
	f := &fakeActions{}
	m := NewMachine(testTarget, f)
	m.Wake()
	if m.State() != Active || f.starts.Load() != 0 {
		t.Fatalf("state %s, starts %d", m.State(), f.starts.Load())
	}
}

func TestOneWakeWithConcurrentConnections(t *testing.T) {
	f := &fakeActions{}
	m := asleepMachine(t, f)
	f.startGate = make(chan struct{})

	const clients = 64
	errs := make(chan error, clients)
	for range clients {
		go func() {
			release, err := m.Serve(nil)
			if err == nil {
				release()
			}
			errs <- err
		}()
	}
	waitState(t, m, Waking)
	close(f.startGate)
	for range clients {
		if err := <-errs; err != nil {
			t.Fatalf("connection: %v", err)
		}
	}
	if got := f.starts.Load(); got != 1 {
		t.Fatalf("%d wakes for %d connections", got, clients)
	}
	if m.State() != Active {
		t.Fatalf("state %s", m.State())
	}
}

func TestConnectionDuringSleepingWakesAtTheEnd(t *testing.T) {
	f := &fakeActions{haltGate: make(chan struct{})}
	m := NewMachine(testTarget, f)
	sleepDone := make(chan error, 1)
	go func() { sleepDone <- m.Sleep() }()
	waitState(t, m, Sleeping)

	served := make(chan error, 1)
	go func() {
		release, err := m.Serve(nil)
		if err == nil {
			release()
		}
		served <- err
	}()
	// The connection waits; nothing starts until the stop is over.
	select {
	case err := <-served:
		t.Fatalf("connection served while stopping: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if f.starts.Load() != 0 {
		t.Fatal("started during SLEEPING")
	}
	close(f.haltGate)
	if err := <-sleepDone; err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	if err := <-served; err != nil {
		t.Fatalf("held connection: %v", err)
	}
	if got := f.starts.Load(); got != 1 {
		t.Fatalf("%d wakes", got)
	}
}

func TestWakePollsUntilItAnswers(t *testing.T) {
	refused := errors.New("refused")
	f := &fakeActions{probeErr: func(attempt int) error {
		if attempt < 5 {
			return refused
		}
		return nil
	}}
	m := asleepMachine(t, f)
	m.Wake()
	waitState(t, m, Active)
	f.mu.Lock()
	attempts := f.attempts
	f.mu.Unlock()
	if attempts != 5 {
		t.Fatalf("%d attempts", attempts)
	}
	// The set must not lose the target before the service can take connections.
	got := f.log()
	if slices.Index(got, "probe") > slices.Index(got, "release") {
		t.Fatalf("released before it answered: %q", got)
	}
}

func TestWakeThatNeverAnswersFails(t *testing.T) {
	refused := errors.New("refused")
	f := &fakeActions{probeErr: func(int) error { return refused }}
	m := asleepMachine(t, f)
	m.probeTimeout = 10 * time.Millisecond
	done := make(chan error, 1)
	go func() {
		_, err := m.Serve(nil)
		done <- err
	}()
	if err := <-done; !errors.Is(err, ErrUnavailable) {
		t.Fatalf("held connection: %v", err)
	}
	if m.State() != Failed || f.failed.Load() != 1 {
		t.Fatalf("state %s, failed %d", m.State(), f.failed.Load())
	}
	// It stays closed, and in the set, without trying again.
	if _, err := m.Serve(nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("later connection: %v", err)
	}
	if got := f.starts.Load(); got != 1 {
		t.Fatalf("%d wakes", got)
	}
	m.Close()
	if !slices.Contains(f.log()[3:], "release") {
		t.Fatalf("Close must take the target out of the set: %q", f.log())
	}
}

func TestDeadProcessEndsTheWakeAtOnce(t *testing.T) {
	f := &fakeActions{probeErr: func(int) error { return ErrDead }}
	m := asleepMachine(t, f)
	m.Wake()
	waitState(t, m, Failed)
	f.mu.Lock()
	attempts := f.attempts
	f.mu.Unlock()
	if attempts != 1 {
		t.Fatalf("%d attempts", attempts)
	}
}

func TestFailedStartAndHalt(t *testing.T) {
	f := &fakeActions{startErr: errors.New("no rootfs")}
	m := asleepMachine(t, f)
	m.Wake()
	waitState(t, m, Failed)

	h := &fakeActions{haltErr: errors.New("stuck")}
	n := NewMachine(testTarget, h)
	if err := n.Sleep(); err == nil || n.State() != Failed || h.failed.Load() != 1 {
		t.Fatalf("Sleep = %v, state %s", err, n.State())
	}
}

func TestDormant(t *testing.T) {
	f := &fakeActions{}
	m := NewDormant(testTarget, f)
	if m.State() != Sleeping {
		t.Fatalf("state %s", m.State())
	}
	if err := m.Settle(); err != nil || m.State() != Asleep {
		t.Fatalf("Settle = %v, state %s", err, m.State())
	}
	m.Wake()
	waitState(t, m, Active)
	want := []string{"hold", "start", "probe", "release", "ready"}
	if got := f.log(); !slices.Equal(got, want) {
		t.Fatalf("order %q, want %q", got, want)
	}
}

func TestDormantWakesIfAConnectionCameBeforeItSettled(t *testing.T) {
	f := &fakeActions{}
	m := NewDormant(testTarget, f)
	m.Wake()
	if err := m.Settle(); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, Active)
}

func TestCloseWaitsForTransitionAndReleases(t *testing.T) {
	f := &fakeActions{startGate: make(chan struct{})}
	m := asleepMachine(t, f)
	m.Wake()
	closed := make(chan struct{})
	go func() {
		m.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close did not wait for the wake")
	case <-time.After(20 * time.Millisecond):
	}
	close(f.startGate)
	<-closed
	if m.State() != Closed {
		t.Fatalf("state %s", m.State())
	}
	if _, err := m.Serve(nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Serve = %v", err)
	}
}

func TestCloseAsleepReleases(t *testing.T) {
	f := &fakeActions{}
	m := asleepMachine(t, f)
	m.Close()
	want := []string{"hold", "sockets", "halt", "release"}
	if got := f.log(); !slices.Equal(got, want) {
		t.Fatalf("order %q, want %q", got, want)
	}
	if m.Close(); len(f.log()) != len(want) {
		t.Fatalf("a second Close released again: %q", f.log())
	}
}

const procTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2194903 1 000000001e7525b8 100 0 0 10 0
   1: 0200510A:1F90 0100510A:9C40 01 00000000:00000000 00:00000000 00000000     0        0 2194904 1 000000001e7525b9 20 4 30 10 -1
   2: 0200510A:1F90 0100510A:9C41 06 00000000:00000000 03:00000F5A 00000000     0        0 0 3 000000001e7525ba
   3: 0200510A:1F90 0100510A:9C42 03 00000000:00000000 01:00000064 00000000     0        0 0 1 000000001e7525bb
   4: 0200510A:1F90 0100510A:9C43 08 00000000:00000000 00:00000000 00000000     0        0 2194905 1 000000001e7525bc 20 4 30 10 -1
`

func TestParseTCPStates(t *testing.T) {
	got, err := parseTCPStates([]byte(procTCP))
	if err != nil {
		t.Fatal(err)
	}
	if want := []uint8{tcpListen, tcpEstablished, tcpTimeWait, tcpSynRecv, 0x08}; !slices.Equal(got, want) {
		t.Fatalf("states %v, want %v", got, want)
	}
	if got, err := parseTCPStates([]byte(strings.SplitAfter(procTCP, "\n")[0])); err != nil || len(got) != 0 {
		t.Fatalf("only a header: %v, %v", got, err)
	}
	for name, bad := range map[string]string{
		"empty":       "",
		"short line":  "header\n   0: 00000000:1F90\n",
		"not hex":     "header\n   0: 00000000:1F90 00000000:0000 ZZ 0\n",
		"state width": "header\n   0: 00000000:1F90 00000000:0000 1FF 0\n",
	} {
		if got, err := parseTCPStates([]byte(bad)); err == nil {
			t.Errorf("%s: got %v", name, got)
		}
	}
}

func TestSummarize(t *testing.T) {
	cases := []struct {
		name   string
		states []uint8
		want   Sockets
	}{
		{"nothing", nil, Sockets{}},
		{"listening is quiet", []uint8{tcpListen, tcpListen}, Sockets{}},
		{"time wait is over", []uint8{tcpListen, tcpTimeWait}, Sockets{}},
		{"established", []uint8{tcpListen, tcpEstablished}, Sockets{Active: 1, Live: 1}},
		{"half open", []uint8{tcpSynRecv}, Sockets{Active: 1, Live: 1}},
		{"close wait is not active but is alive", []uint8{0x08}, Sockets{Live: 1}},
		{"outgoing connect is alive", []uint8{0x02}, Sockets{Live: 1}},
	}
	for _, c := range cases {
		if got := summarize(c.states); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestParseTxPackets(t *testing.T) {
	const dev = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:     656      12    0    0    0     0          0         0      656      13    0    0    0     0       0          0
  eth0:      90       1    0    0    0     0          0         0     1234      77    0    0    0     0       0          0
`
	if got, err := parseTxPackets([]byte(dev), "eth0"); err != nil || got != 77 {
		t.Fatalf("eth0 = %d, %v", got, err)
	}
	if got, err := parseTxPackets([]byte(dev), "lo"); err != nil || got != 13 {
		t.Fatalf("lo = %d, %v", got, err)
	}
	if got, err := parseTxPackets([]byte(dev), "eth1"); err == nil {
		t.Fatalf("eth1 = %d", got)
	}
}

func TestIdle(t *testing.T) {
	t0 := time.Unix(1000, 0)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	active := Sockets{Active: 1, Live: 1}

	i := NewIdle(2*time.Second, t0)
	if i.Interval() != 500*time.Millisecond {
		t.Fatalf("interval %s", i.Interval())
	}
	if i.Observe(at(500), Sockets{}, 7) || i.Observe(at(1500), Sockets{}, 7) {
		t.Fatal("idle before the window")
	}
	if !i.Observe(at(2000), Sockets{}, 7) {
		t.Fatal("not idle after a quiet window")
	}

	i = NewIdle(2*time.Second, t0)
	i.Observe(at(500), Sockets{}, 7)
	if i.Observe(at(2000), active, 7) {
		t.Fatal("an open connection is activity, however quiet")
	}
	if i.Observe(at(3500), Sockets{}, 7) || !i.Observe(at(4000), Sockets{}, 7) {
		t.Fatal("the window restarts when the connection is last seen")
	}

	i = NewIdle(2*time.Second, t0)
	i.Observe(at(500), Sockets{}, 7)
	if i.Observe(at(2000), Sockets{}, 8) {
		t.Fatal("outgoing packets are activity")
	}

	i = NewIdle(2*time.Second, t0)
	i.Observe(at(500), Sockets{}, 7)
	i.Touch(at(1900))
	if i.Observe(at(2500), Sockets{}, 50) {
		t.Fatal("Touch restarts the window")
	}
	if !i.Observe(at(3900), Sockets{}, 50) {
		t.Fatal("the count after Touch is only a baseline")
	}
}

func TestParseSockaddrIn(t *testing.T) {
	// What the kernel returned for 10.81.0.3:8080 when this was measured.
	raw := []byte{0x02, 0x00, 0x1f, 0x90, 0x0a, 0x51, 0x00, 0x03, 0, 0, 0, 0, 0, 0, 0, 0}
	got, err := parseSockaddrIn(raw)
	if err != nil || got != netip.MustParseAddrPort("10.81.0.3:8080") {
		t.Fatalf("got %s, %v", got, err)
	}
	if _, err := parseSockaddrIn(raw[:8]); err == nil {
		t.Fatal("a short sockaddr must fail")
	}
	v6 := slices.Clone(raw)
	v6[0] = 0x0a
	if _, err := parseSockaddrIn(v6); err == nil {
		t.Fatal("another family must fail")
	}
}

func TestScripts(t *testing.T) {
	rules := rulesScript()
	for _, want := range []string{
		"set sleeping { type ipv4_addr . inet_service; }",
		"type nat hook prerouting priority dstnat; ip daddr . tcp dport @sleeping redirect to :15000;",
		"type nat hook output priority dstnat; meta mark 0x414e return; ip daddr . tcp dport @sleeping redirect to :15000;",
	} {
		if !strings.Contains(rules, want) {
			t.Errorf("rules lack %q:\n%s", want, rules)
		}
	}
	if !strings.HasPrefix(rules, "table inet anden_wake {}\ndelete table inet anden_wake\n") {
		t.Errorf("rules must replace an existing table:\n%s", rules)
	}
	if got := elementScript("add", testTarget); got != "add element inet anden_wake sleeping { 10.81.0.2 . 8080 }\n" {
		t.Errorf("add: %q", got)
	}
	if got := elementScript("delete", testTarget); got != "delete element inet anden_wake sleeping { 10.81.0.2 . 8080 }\n" {
		t.Errorf("delete: %q", got)
	}

	var applied []string
	n := &NFT{apply: func(s string) error { applied = append(applied, s); return nil }}
	if err := n.Add(testTarget); err != nil {
		t.Fatal(err)
	}
	if err := n.Delete(testTarget); err != nil {
		t.Fatal(err)
	}
	if len(applied) != 2 {
		t.Fatalf("%d transactions, want one each", len(applied))
	}
}

// tcpPair returns the two ends of a loopback TCP connection.
func tcpPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback: %v", err)
	}
	defer ln.Close()
	client, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})
	return client.(*net.TCPConn), server.(*net.TCPConn)
}

func TestBridgeCopiesBothWaysAndHalfCloses(t *testing.T) {
	client, proxyFront := tcpPair(t)
	proxyBack, server := tcpPair(t)
	done := make(chan struct{})
	go func() {
		bridge(proxyFront, proxyBack)
		close(done)
	}()

	if _, err := client.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(server)
	if err != nil || string(got) != "request" {
		t.Fatalf("server read %q, %v", got, err)
	}
	// The client closed its half; the answer must still get through.
	if _, err := server.Write([]byte("answer")); err != nil {
		t.Fatal(err)
	}
	if err := server.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err = io.ReadAll(client)
	if err != nil || string(got) != "answer" {
		t.Fatalf("client read %q, %v", got, err)
	}
	<-done
}

func TestHandleServesThroughTheMachine(t *testing.T) {
	back, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback: %v", err)
	}
	defer back.Close()
	go func() {
		conn, err := back.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 5)
		if _, err := io.ReadFull(conn, buf); err == nil {
			conn.Write(append([]byte("echo "), buf...))
		}
	}()
	target := netip.MustParseAddrPort(back.Addr().String())

	p := NewProxy()
	// Without the mark, which takes CAP_NET_ADMIN and has no rule to pass here.
	p.dial = func(ctx context.Context, target netip.AddrPort) (*net.TCPConn, error) {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp4", target.String())
		if err != nil {
			return nil, err
		}
		return conn.(*net.TCPConn), nil
	}
	f := &fakeActions{}
	m := NewMachine(target, f)
	if err := m.Sleep(); err != nil {
		t.Fatal(err)
	}
	p.Register(m)
	client, front := tcpPair(t)
	handled := make(chan error, 1)
	go func() {
		handled <- p.handle(context.Background(), front, func(*net.TCPConn) (netip.AddrPort, error) { return target, nil })
	}()

	if _, err := client.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(client)
	if err != nil || string(got) != "echo hello" {
		t.Fatalf("client read %q, %v", got, err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if err := <-handled; err != nil {
		t.Fatalf("handle: %v", err)
	}
	if f.starts.Load() != 1 || m.State() != Active {
		t.Fatalf("starts %d, state %s", f.starts.Load(), m.State())
	}

	p.Unregister(m)
	if p.lookup(target) != nil {
		t.Fatal("still registered")
	}
	client2, front2 := tcpPair(t)
	defer client2.Close()
	err = p.handle(context.Background(), front2, func(*net.TCPConn) (netip.AddrPort, error) { return target, nil })
	if err == nil {
		t.Fatal("a connection to no service must fail")
	}
}
