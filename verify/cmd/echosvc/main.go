// Command echosvc is the test service: it answers every request with a few
// hundred bytes that carry its version and an in-memory counter.
//
// PORT is the TCP port (8080 by default) and VERSION the reported version. If
// LOG_PATH is set, each request with an X-Request-Id header appends a JSON line
// {"id": ...} to that file before the response is written, so an answered
// request is always in the log.
//
// Three paths misbehave on request, for the limit criteria, and answer at once:
// /hog?mib=N allocates and touches N MiB, /fork starts a process that forks
// until the kernel refuses and keeps trying, and /burn?seconds=S&workers=W keeps
// W goroutines on the CPU for S seconds.
//
//go:debug multipathtcp=0
package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Arguments of the processes /fork starts, through /proc/self/exe.
const (
	modeBomb = "bomb"
	modeHold = "hold"
)

const (
	bombRetry = 10 * time.Millisecond
	bombLife  = 5 * time.Minute
)

type response struct {
	Version string `json:"version"`
	Count   uint64 `json:"count"`
	Padding string `json:"padding"`
}

type logLine struct {
	ID string `json:"id"`
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case modeBomb:
			bomb()
		case modeHold:
			time.Sleep(bombLife)
		}
		return
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	var requests io.Writer
	if path := os.Getenv("LOG_PATH"); path != "" {
		file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			log.Fatal(err)
		}
		defer file.Close()
		requests = file
	}
	log.Fatal(http.ListenAndServe(":"+port, newHandler(os.Getenv("VERSION"), requests)))
}

// bomb forks processes that stay alive for as long as it can, and keeps trying
// when the kernel refuses, each refusal counting in pids.events.
func bomb() {
	for end := time.Now().Add(bombLife); time.Now().Before(end); {
		if err := exec.Command("/proc/self/exe", modeHold).Start(); err != nil {
			time.Sleep(bombRetry)
		}
	}
}

// queryInt reads an integer of the query, with a default.
func queryInt(r *http.Request, name string, fallback int) int {
	if v, err := strconv.Atoi(r.URL.Query().Get(name)); err == nil {
		return v
	}
	return fallback
}

// held keeps what /hog allocated reachable.
var held [][]byte

func hog(mib int) {
	buf := make([]byte, mib<<20)
	for i := 0; i < len(buf); i += os.Getpagesize() {
		buf[i] = 1
	}
	held = append(held, buf)
}

func burn(seconds, workers int) {
	end := time.Now().Add(time.Duration(seconds) * time.Second)
	for range workers {
		go func() {
			for time.Now().Before(end) {
			}
		}()
	}
}

func newHandler(version string, requests io.Writer) http.Handler {
	var count atomic.Uint64
	padding := strings.Repeat("x", 256)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hog":
			// Answers first: the kernel kills the process while it allocates.
			w.WriteHeader(http.StatusAccepted)
			w.(http.Flusher).Flush()
			hog(queryInt(r, "mib", 1024))
			return
		case "/fork":
			if err := exec.Command("/proc/self/exe", modeBomb).Start(); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			return
		case "/burn":
			burn(queryInt(r, "seconds", 10), queryInt(r, "workers", 4))
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if id := r.Header.Get("X-Request-Id"); id != "" && requests != nil {
			line, err := json.Marshal(logLine{ID: id})
			if err == nil {
				// One write per line: O_APPEND keeps concurrent lines whole.
				_, err = requests.Write(append(line, '\n'))
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		body, err := json.Marshal(response{Version: version, Count: count.Add(1), Padding: padding})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})
}
