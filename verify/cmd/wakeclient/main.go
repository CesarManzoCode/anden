// Command wakeclient measures a service from outside the product (D11): each
// request is one connection, and the clock runs from before connect() to the
// last byte of the response. A failed request is reported, never retried, so a
// lost request stays visible.
//
// Usage:
//
//	wakeclient measure --addr HOST:PORT [--path P] [--id ID] [--timeout D] [--retry D]
//	wakeclient load --addr HOST:PORT --clients N --requests M [--prefix P] [--path P] [--timeout D]
//	wakeclient sample --addr HOST:PORT [--path P] [--timeout D]
//
// measure makes one request and prints one JSON line. load runs N concurrent
// clients of M requests each, every request with a unique X-Request-Id, and
// prints one JSON line per request. sample prints the version that answered, as
// echosvc reports it. They exit with 0, with 1 if a request failed, and with 2
// if the command line is wrong.
//
// With --retry, measure connects again every millisecond while no connection is
// made, for at most that long, and reports the last attempt: it is for a service
// that is still starting, with no proxy to hold the connection. Every line has
// end_ns, CLOCK_MONOTONIC at the end of the request, so that whoever started
// the service can time it from the start.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	usage          = "usage: wakeclient measure|load|sample --addr HOST:PORT [flags]\n"
	exitOK         = 0
	exitFailed     = 1
	exitUsage      = 2
	defaultTimeout = 30 * time.Second
	retryInterval  = time.Millisecond
)

// result is the line printed for one request. A request that never connected
// has local_port 0.
type result struct {
	ID        string  `json:"id,omitempty"`
	OK        bool    `json:"ok"`
	ClientMS  float64 `json:"client_ms"`
	LocalPort int     `json:"local_port"`
	Status    int     `json:"status"`
	EndNS     int64   `json:"end_ns"`
	Error     string  `json:"error,omitempty"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	cmd := args[0]
	flags := flag.NewFlagSet("wakeclient "+cmd, flag.ContinueOnError)
	flags.SetOutput(stderr)
	addr := flags.String("addr", "", "HOST:PORT of the service")
	path := flags.String("path", "/", "path of the request")
	timeout := flags.Duration("timeout", defaultTimeout, "limit of each request, connect included")
	var id, prefix *string
	var clients, requests *int
	var retry *time.Duration
	switch cmd {
	case "measure":
		id = flags.String("id", "", "X-Request-Id of the request")
		retry = flags.Duration("retry", 0, "connect again while no connection is made, for at most this long")
	case "load":
		prefix = flags.String("prefix", "req", "start of each request id")
		clients = flags.Int("clients", 0, "concurrent clients")
		requests = flags.Int("requests", 0, "requests of each client")
	case "sample":
	default:
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	if err := flags.Parse(args[1:]); err != nil {
		return exitUsage
	}
	if *addr == "" || flags.NArg() != 0 || (cmd == "load" && (*clients <= 0 || *requests <= 0)) {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	switch cmd {
	case "measure":
		return measure(stdout, stderr, *addr, *path, *id, *timeout, *retry)
	case "load":
		return load(stdout, stderr, *addr, *path, *prefix, *clients, *requests, *timeout)
	}
	return sample(stdout, stderr, *addr, *path, *timeout)
}

func measure(stdout, stderr io.Writer, addr, path, id string, timeout, retry time.Duration) int {
	deadline := time.Now().Add(retry)
	res, _ := request(addr, path, id, timeout)
	for res.LocalPort == 0 && time.Now().Before(deadline) {
		time.Sleep(retryInterval)
		res, _ = request(addr, path, id, timeout)
	}
	if err := json.NewEncoder(stdout).Encode(res); err != nil {
		fmt.Fprintln(stderr, "wakeclient:", err)
		return exitFailed
	}
	if !res.OK {
		return exitFailed
	}
	return exitOK
}

func load(stdout, stderr io.Writer, addr, path, prefix string, clients, requests int, timeout time.Duration) int {
	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		failed bool
	)
	enc := json.NewEncoder(stdout)
	for c := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range requests {
				res, _ := request(addr, path, fmt.Sprintf("%s-c%d-r%d", prefix, c, r), timeout)
				mu.Lock()
				err := enc.Encode(res)
				if err != nil {
					fmt.Fprintln(stderr, "wakeclient:", err)
				}
				failed = failed || err != nil || !res.OK
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if failed {
		return exitFailed
	}
	return exitOK
}

func sample(stdout, stderr io.Writer, addr, path string, timeout time.Duration) int {
	res, body := request(addr, path, "", timeout)
	version, err := sampledVersion(res, body)
	if err != nil {
		fmt.Fprintln(stderr, "wakeclient:", err)
		return exitFailed
	}
	fmt.Fprintln(stdout, version)
	return exitOK
}

// sampledVersion reads the version out of what a service answered.
func sampledVersion(res result, body []byte) (string, error) {
	if !res.OK {
		return "", fmt.Errorf("request failed: status %d: %s", res.Status, res.Error)
	}
	var answer struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return "", fmt.Errorf("response: %w", err)
	}
	if answer.Version == "" {
		return "", errors.New("the response has no version")
	}
	return answer.Version, nil
}

// request makes one GET on a connection of its own, which the response closes.
// It returns the body as well, for whoever wants to read it.
func request(addr, path, id string, timeout time.Duration) (result, []byte) {
	res := result{ID: id}
	start := time.Now()
	body, err := exchange(&res, start, addr, path, id, timeout)
	res.ClientMS = float64(time.Since(start)) / float64(time.Millisecond)
	res.EndNS = monotonicNS()
	if err != nil {
		res.Error = err.Error()
		return res, nil
	}
	res.OK = res.Status >= http.StatusOK && res.Status < http.StatusMultipleChoices
	if !res.OK {
		res.Error = http.StatusText(res.Status)
	}
	return res, body
}

// exchange fills the port and the status of res and returns the whole body.
func exchange(res *result, start time.Time, addr, path, id string, timeout time.Duration) ([]byte, error) {
	dialer := net.Dialer{Deadline: start.Add(timeout)}
	conn, err := dialer.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if tcp, ok := conn.LocalAddr().(*net.TCPAddr); ok {
		res.LocalPort = tcp.Port
	}
	if err := conn.SetDeadline(start.Add(timeout)); err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		return nil, err
	}
	req.Close = true
	if id != "" {
		req.Header.Set("X-Request-Id", id)
	}
	if err := req.Write(conn); err != nil {
		return nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	res.Status = resp.StatusCode
	return io.ReadAll(resp.Body)
}

// monotonicNS reads CLOCK_MONOTONIC, the clock of Python's time.monotonic_ns.
// time.Now keeps a monotonic reading but does not expose its value.
func monotonicNS() int64 {
	var ts syscall.Timespec
	const clockMonotonic = 1
	if _, _, e := syscall.RawSyscall(syscall.SYS_CLOCK_GETTIME, clockMonotonic, uintptr(unsafe.Pointer(&ts)), 0); e != 0 {
		return 0
	}
	return ts.Nano()
}
