// Package dns is the name server of andend (contract D22): it answers A
// records for <service>.anden.internal over UDP and TCP, with TTL 1, from the
// addresses the control plane keeps in memory, and NXDOMAIN for any other name.
package dns

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"sync"

	mdns "github.com/miekg/dns"
)

const (
	// Zone is the domain of the services, as a fully qualified name.
	Zone = "anden.internal."
	// TTL bounds how long a client keeps an address that changed.
	TTL = 1
)

// Records is the address of each service, by name. The reconciler replaces it
// after each pass; the server only reads it.
type Records struct {
	mu    sync.RWMutex
	addrs map[string]netip.Addr
}

func NewRecords() *Records {
	return &Records{addrs: map[string]netip.Addr{}}
}

// Replace makes addrs the whole set of names. The map must not change afterwards.
func (r *Records) Replace(addrs map[string]netip.Addr) {
	r.mu.Lock()
	r.addrs = addrs
	r.mu.Unlock()
}

// Lookup returns the address of a service.
func (r *Records) Lookup(service string) (netip.Addr, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	addr, ok := r.addrs[service]
	return addr, ok
}

// Answer builds the reply to req. A known name answers its A record, or no
// record for another type; anything else is NXDOMAIN.
func Answer(req *mdns.Msg, records *Records) *mdns.Msg {
	m := new(mdns.Msg)
	m.SetReply(req)
	if req.Opcode != mdns.OpcodeQuery || len(req.Question) != 1 {
		m.Rcode = mdns.RcodeNotImplemented
		return m
	}
	q := req.Question[0]
	service, inZone := strings.CutSuffix(strings.ToLower(mdns.Fqdn(q.Name)), "."+Zone)
	m.Authoritative = inZone
	var addr netip.Addr
	found := false
	if inZone && !strings.Contains(service, ".") {
		addr, found = records.Lookup(service)
	}
	if !found || q.Qclass != mdns.ClassINET {
		m.Rcode = mdns.RcodeNameError
		return m
	}
	if q.Qtype == mdns.TypeA || q.Qtype == mdns.TypeANY {
		m.Answer = append(m.Answer, &mdns.A{
			Hdr: mdns.RR_Header{Name: q.Name, Rrtype: mdns.TypeA, Class: mdns.ClassINET, Ttl: TTL},
			A:   net.IP(addr.AsSlice()),
		})
	}
	return m
}

// Server answers on one address over UDP and TCP.
type Server struct {
	udp, tcp *mdns.Server
}

// Listen binds UDP and TCP on addr. With port 0 both take the port UDP got.
func Listen(addr string, records *Records) (*Server, error) {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", pc.LocalAddr().String())
	if err != nil {
		pc.Close()
		return nil, err
	}
	handler := mdns.HandlerFunc(func(w mdns.ResponseWriter, req *mdns.Msg) {
		_ = w.WriteMsg(Answer(req, records))
	})
	return &Server{
		udp: &mdns.Server{PacketConn: pc, Handler: handler},
		tcp: &mdns.Server{Listener: ln, Handler: handler},
	}, nil
}

// Addr is the address both transports listen on.
func (s *Server) Addr() string {
	return s.udp.PacketConn.LocalAddr().String()
}

// Serve answers until ctx ends or a transport fails.
func (s *Server) Serve(ctx context.Context) error {
	servers := []*mdns.Server{s.udp, s.tcp}
	errc := make(chan error, len(servers))
	for _, srv := range servers {
		go func() { errc <- srv.ActivateAndServe() }()
	}
	var err error
	pending := len(servers)
	select {
	case <-ctx.Done():
	case err = <-errc:
		pending--
	}
	for _, srv := range servers {
		// Shutdown refuses a server that has not started yet; closing its
		// sockets ends it as soon as it does.
		_ = srv.Shutdown()
	}
	s.udp.PacketConn.Close()
	s.tcp.Listener.Close()
	for ; pending > 0; pending-- {
		<-errc
	}
	if ctx.Err() != nil {
		return nil
	}
	return err
}
