package wake

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"
)

// contractEvent is the line of section 5 of the contract.
const contractEvent = `{"service": "s", "route": "relaunch", "route_requested": "restore", "client_addr": "10.80.0.100:40112", "t_accept_ns": 0, "phases_ms": {"detection": 0.0, "placement": 0.0, "rootfs": 0.0, "network": 0.0, "start": 0.0, "readiness": 0.0, "first_byte": 0.0}}`

var allPhases = []Phase{Detection, Placement, Rootfs, Network, Start, Readiness, FirstByte}

func phaseValues(ms PhasesMS) []float64 {
	return []float64{ms.Detection, ms.Placement, ms.Rootfs, ms.Network, ms.Start, ms.Readiness, ms.FirstByte}
}

func sumMS(ms PhasesMS) float64 {
	total := 0.0
	for _, v := range phaseValues(ms) {
		total += v
	}
	return total
}

func totalMS(p *Phases) float64 { return float64(p.Total()) / float64(time.Millisecond) }

func TestPhasesRecordSevenAndAddUpToTheTotal(t *testing.T) {
	p := NewPhases("10.80.0.100:40112")
	at := p.accept
	for i, ph := range allPhases {
		at = at.Add(time.Duration(i+1) * time.Millisecond)
		p.mark(ph, at)
	}
	got := phaseValues(p.Millis())
	want := []float64{1, 2, 3, 4, 5, 6, 7}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("phases %v, want %v", got, want)
	}
	if sum, total := sumMS(p.Millis()), totalMS(p); sum != total || total != 28 {
		t.Fatalf("sum %v, total %v, want 28", sum, total)
	}
}

func TestPhasesMarkedAgainAdd(t *testing.T) {
	p := NewPhases("")
	at := p.accept
	for _, step := range []struct {
		ph Phase
		d  time.Duration
	}{{Rootfs, 2 * time.Millisecond}, {Start, 3 * time.Millisecond}, {Rootfs, 4 * time.Millisecond}} {
		at = at.Add(step.d)
		p.mark(step.ph, at)
	}
	ms := p.Millis()
	if ms.Rootfs != 6 || ms.Start != 3 || sumMS(ms) != totalMS(p) {
		t.Fatalf("rootfs %v, start %v, sum %v, total %v", ms.Rootfs, ms.Start, sumMS(ms), totalMS(p))
	}
}

func TestPhasesOnTheRealClockAddUpToTheTotal(t *testing.T) {
	p := NewPhases("")
	for _, ph := range allPhases {
		time.Sleep(100 * time.Microsecond)
		p.Mark(ph)
	}
	for i, v := range phaseValues(p.Millis()) {
		if v <= 0 {
			t.Fatalf("%s is %v ms", allPhases[i], v)
		}
	}
	if sum, total := sumMS(p.Millis()), totalMS(p); math.Abs(sum-total) > 1e-6 {
		t.Fatalf("sum %v, total %v", sum, total)
	}
}

func TestNilPhasesTakeMarks(t *testing.T) {
	var p *Phases
	p.Mark(Start)
	if p.Woke() {
		t.Fatal("a nil breakdown woke nothing")
	}
}

func TestPhaseNames(t *testing.T) {
	want := []string{"detection", "placement", "rootfs", "network", "start", "readiness", "first_byte"}
	for i, ph := range allPhases {
		if ph.String() != want[i] {
			t.Fatalf("phase %d is %q, want %q", i, ph, want[i])
		}
	}
}

func TestEventLineMatchesTheContract(t *testing.T) {
	ev := Event{Service: "s", Route: "relaunch", RouteRequested: "restore", ClientAddr: "10.80.0.100:40112"}
	line, err := ev.Line()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(line, []byte("\n")) || bytes.Count(line, []byte("\n")) != 1 {
		t.Fatalf("not one line: %q", line)
	}
	var got, want map[string]any
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(contractEvent), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("event %s, want %s", line, contractEvent)
	}
	// Same keys in the same order as the contract.
	compact := new(bytes.Buffer)
	if err := json.Compact(compact, []byte(contractEvent)); err != nil {
		t.Fatal(err)
	}
	// Go writes 0.0 as 0.
	want0 := strings.NewReplacer("0.0,", "0,", "0.0}", "0}").Replace(compact.String())
	if string(bytes.TrimSpace(line)) != want0 {
		t.Fatalf("event %s, want the order of %s", line, compact)
	}
}

func TestEventCarriesThePhases(t *testing.T) {
	p := NewPhases("10.80.0.100:1")
	p.mark(Start, p.accept.Add(1500*time.Microsecond))
	ev := p.Event("s", "restore", "restore")
	if ev.ClientAddr != "10.80.0.100:1" || ev.PhasesMS.Start != 1.5 || ev.TAcceptNS <= 0 {
		t.Fatalf("event %+v", ev)
	}
}

// slowActions spaces the marks of a start; with a refused first probe and a
// slow answer, every phase of a wake takes some time.
type slowActions struct {
	*fakeActions
}

func (s slowActions) Start(phases *Phases) error {
	s.starts.Add(1)
	for _, ph := range []Phase{Placement, Rootfs, Network, Start} {
		time.Sleep(time.Millisecond)
		phases.Mark(ph)
	}
	return nil
}

func TestWakeOfAConnectionRecordsSevenPhases(t *testing.T) {
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
			time.Sleep(time.Millisecond)
			conn.Write(append([]byte("echo "), buf...))
		}
	}()
	target := netip.MustParseAddrPort(back.Addr().String())
	p := NewProxy()
	p.dial = func(ctx context.Context, target netip.AddrPort) (*net.TCPConn, error) {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp4", target.String())
		if err != nil {
			return nil, err
		}
		return conn.(*net.TCPConn), nil
	}
	f := &fakeActions{recorded: make(chan *Phases, 2), probeErr: func(attempt int) error {
		if attempt == 1 {
			return io.ErrUnexpectedEOF
		}
		return nil
	}}
	m := NewMachine(target, slowActions{f})
	if err := m.Sleep(); err != nil {
		t.Fatal(err)
	}
	p.Register(m)
	client, front := tcpPair(t)
	phases := NewPhases(front.RemoteAddr().String())
	time.Sleep(time.Millisecond)
	handled := make(chan error, 1)
	go func() {
		handled <- p.handle(context.Background(), front, phases, func(*net.TCPConn) (netip.AddrPort, error) { return target, nil })
	}()
	if _, err := client.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(client)
	if err != nil || string(got) != "echo hello" {
		t.Fatalf("client read %q, %v", got, err)
	}
	client.CloseWrite()
	if err := <-handled; err != nil {
		t.Fatalf("handle: %v", err)
	}

	var rec *Phases
	select {
	case rec = <-f.recorded:
	case <-time.After(5 * time.Second):
		t.Fatal("the wake was not recorded")
	}
	if rec != phases || rec.Client != front.RemoteAddr().String() {
		t.Fatalf("recorded %p for %q, want the connection's %p", rec, rec.Client, phases)
	}
	ms := rec.Millis()
	for i, v := range phaseValues(ms) {
		if v <= 0 {
			t.Fatalf("%s is %v ms", allPhases[i], v)
		}
	}
	if sum, total := sumMS(ms), totalMS(rec); math.Abs(sum-total) > 1e-6 {
		t.Fatalf("sum %v, total %v", sum, total)
	}
	select {
	case again := <-f.recorded:
		t.Fatalf("a second record: %+v", again.Millis())
	case <-time.After(50 * time.Millisecond):
	}
}

func TestWakeWithoutAConnectionIsRecordedWhenActive(t *testing.T) {
	f := &fakeActions{recorded: make(chan *Phases, 1)}
	m := asleepMachine(t, f)
	m.Wake()
	waitState(t, m, Active)
	select {
	case rec := <-f.recorded:
		if rec.Client != "" || rec.Woke() || rec.Millis().FirstByte != 0 {
			t.Fatalf("recorded %q woke=%v %+v", rec.Client, rec.Woke(), rec.Millis())
		}
		if sum, total := sumMS(rec.Millis()), totalMS(rec); math.Abs(sum-total) > 1e-6 {
			t.Fatalf("sum %v, total %v", sum, total)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wake was not recorded")
	}
}

func TestOnlyTheFirstConnectionOwnsTheWake(t *testing.T) {
	f := &fakeActions{startGate: make(chan struct{})}
	m := asleepMachine(t, f)
	first, second := NewPhases("a:1"), NewPhases("b:2")
	owners := make(chan *Phases, 2)
	serve := func(p *Phases) {
		release, owner, err := m.ServeWake(nil, p)
		if err != nil {
			t.Error(err)
			owners <- nil
			return
		}
		release()
		if owner {
			owners <- p
		} else {
			owners <- nil
		}
	}
	go serve(first)
	waitState(t, m, Waking)
	go serve(second)
	close(f.startGate)
	got := []*Phases{<-owners, <-owners}
	if !((got[0] == first && got[1] == nil) || (got[0] == nil && got[1] == first)) {
		t.Fatalf("owners %v, want only the first connection", got)
	}
}
