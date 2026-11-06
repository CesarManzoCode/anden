// Command echosvc is the test service: it answers every request with a few
// hundred bytes that carry its version and an in-memory counter.
//
// PORT is the TCP port (8080 by default) and VERSION the reported version. If
// LOG_PATH is set, each request with an X-Request-Id header appends a JSON line
// {"id": ...} to that file before the response is written, so an answered
// request is always in the log.
//
//go:debug multipathtcp=0
package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
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

func newHandler(version string, requests io.Writer) http.Handler {
	var count atomic.Uint64
	padding := strings.Repeat("x", 256)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
