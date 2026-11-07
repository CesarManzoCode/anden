package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func serve(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "http://")
}

func parseLines(t *testing.T, out string) []result {
	t.Helper()
	var lines []result
	for _, raw := range strings.Split(strings.TrimSpace(out), "\n") {
		var res result
		if err := json.Unmarshal([]byte(raw), &res); err != nil {
			t.Fatalf("line %q: %v", raw, err)
		}
		lines = append(lines, res)
	}
	return lines
}

func runArgs(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	rc := run(args, &stdout, &stderr)
	return rc, stdout.String(), stderr.String()
}

func TestMeasurePrintsTimePortAndResult(t *testing.T) {
	var header, remotePort string
	var closing bool
	addr := serve(t, func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Get("X-Request-Id")
		closing = r.Close
		_, remotePort, _ = net.SplitHostPort(r.RemoteAddr)
		fmt.Fprint(w, "hello")
	})
	rc, out, _ := runArgs("measure", "--addr", addr, "--id", "one")
	if rc != exitOK {
		t.Fatalf("rc %d, output %s", rc, out)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id", "ok", "client_ms", "local_port", "status"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("no %s in %s", key, out)
		}
	}
	res := parseLines(t, out)[0]
	if !res.OK || res.Status != http.StatusOK || res.ClientMS <= 0 || res.Error != "" {
		t.Errorf("result %+v", res)
	}
	if fmt.Sprint(res.LocalPort) != remotePort {
		t.Errorf("local port %d, the server saw %s", res.LocalPort, remotePort)
	}
	if header != "one" {
		t.Errorf("X-Request-Id %q", header)
	}
	if !closing {
		t.Error("the request did not ask to close its connection")
	}
}

func TestMeasureReportsHTTPError(t *testing.T) {
	addr := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	rc, out, _ := runArgs("measure", "--addr", addr)
	res := parseLines(t, out)[0]
	if rc != exitFailed || res.OK || res.Status != http.StatusInternalServerError || res.Error == "" {
		t.Errorf("rc %d, result %+v", rc, res)
	}
}

func TestMeasureReportsRefusedConnection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	rc, out, _ := runArgs("measure", "--addr", addr)
	res := parseLines(t, out)[0]
	if rc != exitFailed || res.OK || res.Error == "" || res.LocalPort != 0 {
		t.Errorf("rc %d, result %+v", rc, res)
	}
}

func TestMeasureRetriesUntilTheServiceListens(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	started := make(chan *http.Server, 1)
	go func() {
		time.Sleep(30 * time.Millisecond)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			started <- nil
			return
		}
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "up") })}
		started <- server
		server.Serve(ln)
	}()
	rc, out, _ := runArgs("measure", "--addr", addr, "--retry", "5s")
	if server := <-started; server != nil {
		defer server.Close()
	} else {
		t.Skip("the port was taken meanwhile")
	}
	res := parseLines(t, out)[0]
	if rc != exitOK || !res.OK || res.EndNS <= 0 {
		t.Errorf("rc %d, result %+v", rc, res)
	}
}

func TestMeasureTimesOutOnSilentServer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	rc, out, _ := runArgs("measure", "--addr", listener.Addr().String(), "--timeout", "50ms")
	res := parseLines(t, out)[0]
	if rc != exitFailed || res.OK || res.Error == "" || res.LocalPort == 0 {
		t.Errorf("rc %d, result %+v", rc, res)
	}
}

func TestLoadGivesEveryRequestAUniqueID(t *testing.T) {
	const clients, requests = 3, 4
	var mu sync.Mutex
	seen := map[string]int{}
	addr := serve(t, func(_ http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		seen[r.Header.Get("X-Request-Id")]++
	})
	rc, out, _ := runArgs("load", "--addr", addr, "--clients", "3", "--requests", "4", "--prefix", "r0")
	if rc != exitOK {
		t.Fatalf("rc %d, output %s", rc, out)
	}
	lines := parseLines(t, out)
	if len(lines) != clients*requests {
		t.Fatalf("%d lines", len(lines))
	}
	for _, res := range lines {
		if !res.OK || !strings.HasPrefix(res.ID, "r0-") || seen[res.ID] != 1 {
			t.Errorf("result %+v, seen %d times", res, seen[res.ID])
		}
	}
	if len(seen) != clients*requests {
		t.Errorf("the server saw %d ids", len(seen))
	}
}

func TestLoadFailsIfOneRequestFails(t *testing.T) {
	addr := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Request-Id") == "req-c0-r1" {
			w.WriteHeader(http.StatusBadGateway)
		}
	})
	rc, out, _ := runArgs("load", "--addr", addr, "--clients", "1", "--requests", "3")
	lines := parseLines(t, out)
	if rc != exitFailed || len(lines) != 3 {
		t.Fatalf("rc %d, output %s", rc, out)
	}
	for _, res := range lines {
		if res.OK == (res.ID == "req-c0-r1") {
			t.Errorf("result %+v", res)
		}
	}
}

func TestSamplePrintsVersion(t *testing.T) {
	addr := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"version":"v7","count":1}`)
	})
	rc, out, _ := runArgs("sample", "--addr", addr)
	if rc != exitOK || out != "v7\n" {
		t.Errorf("rc %d, output %q", rc, out)
	}
}

func TestSampleErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"not JSON", http.StatusOK, "hello"},
		{"no version", http.StatusOK, `{"count":1}`},
		{"http error", http.StatusInternalServerError, `{"version":"v7"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			addr := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.status)
				fmt.Fprint(w, c.body)
			})
			rc, out, stderr := runArgs("sample", "--addr", addr)
			if rc != exitFailed || out != "" || stderr == "" {
				t.Errorf("rc %d, stdout %q, stderr %q", rc, out, stderr)
			}
		})
	}
}

func TestUsageErrors(t *testing.T) {
	cases := [][]string{
		{},
		{"nope", "--addr", "x:1"},
		{"measure"},
		{"measure", "--addr", "x:1", "extra"},
		{"measure", "--unknown"},
		{"load", "--addr", "x:1"},
		{"load", "--addr", "x:1", "--clients", "2", "--requests", "0"},
		{"sample"},
	}
	for _, args := range cases {
		if rc, _, _ := runArgs(args...); rc != exitUsage {
			t.Errorf("%v: rc %d", args, rc)
		}
	}
}
