package wake

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"syscall"
	"unsafe"
)

const (
	soOriginalDst = 80
	sockaddrInLen = 16
	// firstReadSize is the buffer of the first read of an answer.
	firstReadSize = 32 << 10
)

// Proxy accepts what the rules redirect to :15000, holds each connection until
// its service is Active and then joins it to the service.
type Proxy struct {
	dial     func(context.Context, netip.AddrPort) (*net.TCPConn, error)
	mu       sync.RWMutex
	machines map[netip.AddrPort]*Machine
}

func NewProxy() *Proxy {
	return &Proxy{dial: Dial, machines: map[netip.AddrPort]*Machine{}}
}

// Register makes connections to the machine's target its own.
func (p *Proxy) Register(m *Machine) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.machines[m.Target()] = m
}

// Unregister forgets a machine, unless another one has taken its target.
func (p *Proxy) Unregister(m *Machine) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.machines[m.Target()] == m {
		delete(p.machines, m.Target())
	}
}

func (p *Proxy) lookup(target netip.AddrPort) *Machine {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.machines[target]
}

// Listen opens :15000. Without MPTCP, which Go enables by default on listeners:
// io.Copy could not use splice on those sockets.
func Listen(ctx context.Context) (*net.TCPListener, error) {
	var lc net.ListenConfig
	lc.SetMultipathTCP(false)
	ln, err := lc.Listen(ctx, "tcp4", ":"+strconv.Itoa(ListenPort))
	if err != nil {
		return nil, err
	}
	return ln.(*net.TCPListener), nil
}

// Serve accepts until ctx ends. Each connection has its own goroutine and its
// own wait.
func (p *Proxy) Serve(ctx context.Context, ln *net.TCPListener) error {
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	for {
		conn, err := ln.AcceptTCP()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		phases := NewPhases(conn.RemoteAddr().String())
		go func() {
			if err := p.handle(ctx, conn, phases, originalDst); err != nil && ctx.Err() == nil {
				log.Printf("wake: %s: %v", conn.RemoteAddr(), err)
			}
		}()
	}
}

// handle serves one connection, accepted when phases began. If it woke the
// service, its wake is recorded once the first byte of the service reaches it,
// or when it ends without one.
func (p *Proxy) handle(ctx context.Context, client *net.TCPConn, phases *Phases, dst func(*net.TCPConn) (netip.AddrPort, error)) error {
	defer client.Close()
	target, err := dst(client)
	if err != nil {
		return err
	}
	m := p.lookup(target)
	if m == nil {
		return fmt.Errorf("no service at %s", target)
	}
	release, owner, err := m.ServeWake(ctx.Done(), phases)
	if err != nil {
		return fmt.Errorf("%s: %w", target, err)
	}
	defer release()
	var first func()
	if owner {
		var once sync.Once
		// On its own goroutine: writing the event must not delay the rest of
		// the answer, which the client is still timing.
		record := func() { once.Do(func() { go m.act.Record(phases) }) }
		defer record()
		first = func() {
			phases.Mark(FirstByte)
			record()
		}
	}
	server, err := p.dial(ctx, target)
	if err != nil {
		return err
	}
	defer server.Close()
	bridge(client, server, first)
	return nil
}

// Dial connects from the node to a service, past the rule of the sleeping set.
func Dial(ctx context.Context, target netip.AddrPort) (*net.TCPConn, error) {
	d := net.Dialer{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_MARK, proxyMark)
		}); err != nil {
			return err
		}
		return serr
	}}
	conn, err := d.DialContext(ctx, "tcp4", target.String())
	if err != nil {
		return nil, err
	}
	return conn.(*net.TCPConn), nil
}

// bridge copies bytes both ways until both are finished. An end of file travels
// on as a half close; an error ends both directions at once. first, if not nil,
// is called once the first bytes from b have been written to a.
func bridge(a, b *net.TCPConn, first func()) {
	errs := make(chan error, 2)
	half := func(dst, src *net.TCPConn, first func()) {
		var err error
		if first != nil {
			err = copyFirst(dst, src, first)
		}
		if err == nil {
			_, err = io.Copy(dst, src)
		}
		if err == nil {
			err = dst.CloseWrite()
		}
		errs <- err
	}
	go half(a, b, first)
	go half(b, a, nil)
	if err := <-errs; err != nil {
		a.Close()
		b.Close()
	}
	<-errs
}

// copyFirst copies one read from src to dst and then calls first. io.Copy
// takes the rest, with splice. An end of file before any byte is left for
// io.Copy to find again.
func copyFirst(dst, src *net.TCPConn, first func()) error {
	buf := make([]byte, firstReadSize)
	n, err := src.Read(buf)
	if n > 0 {
		if _, werr := dst.Write(buf[:n]); werr != nil {
			return werr
		}
		first()
	}
	if err == io.EOF {
		return nil
	}
	return err
}

// originalDst reads where a redirected connection was going: getsockopt(SOL_IP,
// SO_ORIGINAL_DST) fills a sockaddr_in. The syscall package has no call for it.
func originalDst(conn *net.TCPConn) (netip.AddrPort, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return netip.AddrPort{}, err
	}
	var sa [sockaddrInLen]byte
	size := uint32(len(sa))
	var errno syscall.Errno
	err = raw.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd, syscall.SOL_IP, soOriginalDst,
			uintptr(unsafe.Pointer(&sa)), uintptr(unsafe.Pointer(&size)), 0)
	})
	if err != nil {
		return netip.AddrPort{}, err
	}
	if errno != 0 {
		return netip.AddrPort{}, fmt.Errorf("SO_ORIGINAL_DST: %w", errno)
	}
	return parseSockaddrIn(sa[:size])
}

// parseSockaddrIn reads a sockaddr_in: family in host order, then port and
// address in network order.
func parseSockaddrIn(sa []byte) (netip.AddrPort, error) {
	if len(sa) != sockaddrInLen {
		return netip.AddrPort{}, fmt.Errorf("sockaddr_in of %d bytes", len(sa))
	}
	if family := binary.NativeEndian.Uint16(sa[:2]); family != syscall.AF_INET {
		return netip.AddrPort{}, errors.New("sockaddr is not AF_INET")
	}
	addr := netip.AddrFrom4([4]byte(sa[4:8]))
	return netip.AddrPortFrom(addr, binary.BigEndian.Uint16(sa[2:4])), nil
}
