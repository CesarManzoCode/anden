// Command writersvc is the singleton of the partition criterion: every 50 ms it
// appends {"writer": <instance>, "t_ns": <CLOCK_MONOTONIC>} to a file of a
// volume, so that two instances writing at once show up as overlapping beats.
//
// LOG_PATH is the file (required), opened with O_APPEND and written with one
// call per line, so that lines of two writers never mix. PORT is the TCP port
// (8080 by default), where each connection receives the writer's name and is
// closed. The name is WRITER or, without it, the hostname followed by random
// hex: each process is a different instance.
//
//go:debug multipathtcp=0
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"syscall"
	"time"
	"unsafe"
)

const beatInterval = 50 * time.Millisecond

type beat struct {
	Writer string `json:"writer"`
	TNS    int64  `json:"t_ns"`
}

func main() {
	path := os.Getenv("LOG_PATH")
	if path == "" {
		log.Fatal("LOG_PATH is required")
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	writer := os.Getenv("WRITER")
	if writer == "" {
		writer = instanceName()
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatal(err)
	}
	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		log.Fatal(err)
	}
	go serve(ln, writer)
	tick := time.NewTicker(beatInterval)
	defer tick.Stop()
	for {
		line, err := json.Marshal(beat{Writer: writer, TNS: monotonicNS()})
		if err != nil {
			log.Fatal(err)
		}
		if _, err := file.Write(append(line, '\n')); err != nil {
			log.Fatal(err)
		}
		<-tick.C
	}
}

// serve answers each connection with the writer's name.
func serve(ln net.Listener, writer string) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go func() {
			defer conn.Close()
			fmt.Fprintln(conn, writer)
		}()
	}
}

func instanceName() string {
	host, err := os.Hostname()
	if err != nil {
		host = "writer"
	}
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		log.Fatal(err)
	}
	return host + "-" + hex.EncodeToString(b[:])
}

// monotonicNS reads CLOCK_MONOTONIC, the clock of Python's time.monotonic_ns.
// time.Now keeps a monotonic reading but does not expose its value.
func monotonicNS() int64 {
	var ts syscall.Timespec
	const clockMonotonic = 1
	if _, _, e := syscall.RawSyscall(syscall.SYS_CLOCK_GETTIME, clockMonotonic, uintptr(unsafe.Pointer(&ts)), 0); e != 0 {
		log.Fatal(e)
	}
	return ts.Nano()
}
