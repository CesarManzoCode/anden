package dns

import (
	"context"
	"net/netip"
	"testing"
	"time"

	mdns "github.com/miekg/dns"
)

func testRecords() *Records {
	r := NewRecords()
	r.Replace(map[string]netip.Addr{"web": netip.MustParseAddr("10.82.0.3")})
	return r
}

func query(name string, qtype uint16) *mdns.Msg {
	m := new(mdns.Msg)
	m.SetQuestion(name, qtype)
	return m
}

func checkA(t *testing.T, resp *mdns.Msg, want string) {
	t.Helper()
	if resp.Rcode != mdns.RcodeSuccess || !resp.Authoritative || len(resp.Answer) != 1 {
		t.Fatalf("reply: %v", resp)
	}
	a, ok := resp.Answer[0].(*mdns.A)
	if !ok || a.A.String() != want || a.Hdr.Ttl != TTL || a.Hdr.Ttl != 1 || a.Hdr.Class != mdns.ClassINET {
		t.Fatalf("answer: %v", resp.Answer[0])
	}
}

func TestAnswerA(t *testing.T) {
	checkA(t, Answer(query("web.anden.internal.", mdns.TypeA), testRecords()), "10.82.0.3")
	// Names are case insensitive.
	checkA(t, Answer(query("WEB.Anden.Internal.", mdns.TypeA), testRecords()), "10.82.0.3")
}

func TestAnswerNXDOMAIN(t *testing.T) {
	for _, name := range []string{"other.anden.internal.", "anden.internal.", "x.web.anden.internal.", "web.example.com.", "web."} {
		resp := Answer(query(name, mdns.TypeA), testRecords())
		if resp.Rcode != mdns.RcodeNameError || len(resp.Answer) != 0 {
			t.Errorf("%s: %v", name, resp)
		}
	}
}

func TestAnswerOtherTypeHasNoRecord(t *testing.T) {
	resp := Answer(query("web.anden.internal.", mdns.TypeAAAA), testRecords())
	if resp.Rcode != mdns.RcodeSuccess || len(resp.Answer) != 0 {
		t.Fatalf("%v", resp)
	}
}

func TestReplaceChangesTheAnswer(t *testing.T) {
	r := testRecords()
	r.Replace(map[string]netip.Addr{"web": netip.MustParseAddr("10.83.0.2")})
	checkA(t, Answer(query("web.anden.internal.", mdns.TypeA), r), "10.83.0.2")
	r.Replace(map[string]netip.Addr{})
	if resp := Answer(query("web.anden.internal.", mdns.TypeA), r); resp.Rcode != mdns.RcodeNameError {
		t.Fatalf("a removed name: %v", resp)
	}
}

func TestServeUDPAndTCP(t *testing.T) {
	srv, err := Listen("127.0.0.1:0", testRecords())
	if err != nil {
		t.Skipf("no loopback: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	for _, network := range []string{"udp", "tcp"} {
		c := &mdns.Client{Net: network, Timeout: 2 * time.Second}
		resp, _, err := c.Exchange(query("web.anden.internal.", mdns.TypeA), srv.Addr())
		if err != nil {
			t.Fatalf("%s: %v", network, err)
		}
		checkA(t, resp, "10.82.0.3")
		resp, _, err = c.Exchange(query("nope.anden.internal.", mdns.TypeA), srv.Addr())
		if err != nil || resp.Rcode != mdns.RcodeNameError {
			t.Fatalf("%s: %v %v", network, resp, err)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
}
