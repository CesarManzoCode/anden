package wake

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"
)

// TCP states as /proc/net/tcp prints them (include/net/tcp_states.h).
const (
	tcpEstablished = 0x01
	tcpSynRecv     = 0x03
	tcpTimeWait    = 0x06
	tcpListen      = 0x0a
)

// Sockets summarizes the TCP sockets of a network namespace.
type Sockets struct {
	// Active counts ESTABLISHED and SYN_RECV, the signal of D05.
	Active int
	// Live counts all but LISTEN and TIME_WAIT, the ones that abort a sleep. A
	// TIME_WAIT socket has no process or data behind it, and it would keep a
	// service that closed its connection first awake for 60 s.
	Live int
}

// parseTCPStates returns the state column of /proc/net/tcp or tcp6.
func parseTCPStates(data []byte) ([]uint8, error) {
	_, body, found := bytes.Cut(data, []byte("\n"))
	if !found {
		return nil, errors.New("proc net tcp without a header")
	}
	var states []uint8
	for line := range strings.Lines(string(body)) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 4 {
			return nil, fmt.Errorf("proc net tcp line %q", line)
		}
		state, err := strconv.ParseUint(fields[3], 16, 8)
		if err != nil {
			return nil, fmt.Errorf("proc net tcp state: %w", err)
		}
		states = append(states, uint8(state))
	}
	return states, nil
}

func summarize(states []uint8) Sockets {
	var s Sockets
	for _, st := range states {
		switch st {
		case tcpEstablished, tcpSynRecv:
			s.Active++
			s.Live++
		case tcpListen, tcpTimeWait:
		default:
			s.Live++
		}
	}
	return s
}

// ReadSockets reads the sockets of the network namespace of pid. A v4 client of
// a dual stack listener shows up in tcp6, so both files count; tcp6 is missing
// only where the kernel has no IPv6.
func ReadSockets(pid int) (Sockets, error) {
	var all []uint8
	for _, name := range []string{"tcp", "tcp6"} {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/net/%s", pid, name))
		if errors.Is(err, fs.ErrNotExist) && name == "tcp6" {
			continue
		}
		if err != nil {
			return Sockets{}, err
		}
		states, err := parseTCPStates(data)
		if err != nil {
			return Sockets{}, err
		}
		all = append(all, states...)
	}
	return summarize(all), nil
}

// parseTxPackets returns the packets iface has sent, from /proc/net/dev: eight
// receive columns come before transmit bytes and transmit packets.
func parseTxPackets(data []byte, iface string) (uint64, error) {
	for line := range strings.Lines(string(data)) {
		name, counters, found := strings.Cut(line, ":")
		if !found || strings.TrimSpace(name) != iface {
			continue
		}
		fields := strings.Fields(counters)
		if len(fields) < 10 {
			return 0, fmt.Errorf("proc net dev line %q", line)
		}
		return strconv.ParseUint(fields[9], 10, 64)
	}
	return 0, fmt.Errorf("no interface %s in proc net dev", iface)
}

// ReadTxPackets reads what iface in the network namespace of pid has sent. The
// veth of an instance is seen from inside: /sys shows the node's namespace at
// best, and what its end sends is what the service sends.
func ReadTxPackets(pid int, iface string) (uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/net/dev", pid))
	if err != nil {
		return 0, err
	}
	return parseTxPackets(data, iface)
}

// Idle decides D05: inactive if for a whole window there was no active socket
// and the packets sent did not grow. Between samples it can only miss a socket,
// not the packets that socket sent.
type Idle struct {
	window time.Duration
	last   time.Time
	tx     uint64
	seen   bool
}

func NewIdle(window time.Duration, now time.Time) *Idle {
	return &Idle{window: window, last: now}
}

// Interval is how often to sample: a quarter of the window.
func (i *Idle) Interval() time.Duration { return i.window / 4 }

// Touch restarts the window, with a new baseline of packets on the next sample.
func (i *Idle) Touch(now time.Time) {
	i.last, i.seen = now, false
}

// Observe takes one sample and says whether the window has passed without
// activity.
func (i *Idle) Observe(now time.Time, sockets Sockets, tx uint64) bool {
	if sockets.Active > 0 || (i.seen && tx != i.tx) {
		i.last = now
	}
	i.tx, i.seen = tx, true
	return now.Sub(i.last) >= i.window
}
