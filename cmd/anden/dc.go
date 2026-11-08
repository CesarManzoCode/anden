package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"

	andenv1 "github.com/CesarManzoCode/anden/gen/anden/v1"
	"github.com/CesarManzoCode/anden/internal/sim"
)

// runDC handles up, down, exec, status, chaos and the internal _dc and
// _dc-init of the simulator.
func runDC(cmd string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("anden "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	state := fs.String("state", defaultState, "state directory")
	jsonOut := fs.Bool("json", false, "print a JSON object on stdout")
	var ns, sockets *string
	var timeout *time.Duration
	var delayMs *int
	switch cmd {
	case "up":
		timeout = fs.Duration("timeout", 120*time.Second, "how long to wait for the three nodes")
	case "status":
		timeout = fs.Duration("timeout", 10*time.Second, "timeout of the calls")
	case "exec":
		ns = fs.String("ns", "", "client, root or nodeN")
	case "chaos":
		delayMs = fs.Int("delay-ms", 0, "delay that slow adds to what goes to the node, in ms")
	case "_dc", "_dc-init":
		sockets = fs.String("sockets", "", "directory of the Unix sockets")
	}
	// chaos takes its action and node before the flags, as in the contract.
	var positional []string
	if cmd == "chaos" {
		if len(args) < 2 {
			fmt.Fprint(stderr, usage)
			return 2
		}
		positional, args = args[:2], args[2:]
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()

	var err error
	var result any
	switch cmd {
	case "up":
		result, err = sim.Up(*state, *timeout, stderr)
	case "down":
		err = sim.Down(*state)
		result = map[string]any{"down": err == nil}
	case "exec":
		if *ns == "" || len(rest) == 0 {
			fmt.Fprint(stderr, usage)
			return 2
		}
		code, err := sim.Exec(*state, *ns, rest, os.Stdin, stdout, stderr)
		if err != nil {
			fmt.Fprintln(stderr, "anden:", err)
		}
		return code
	case "status":
		return runStatus(*state, *timeout, *jsonOut, stdout, stderr)
	case "chaos":
		action, node := positional[0], positional[1]
		if len(rest) != 0 {
			fmt.Fprint(stderr, usage)
			return 2
		}
		out := map[string]any{"action": action, "node": node}
		switch action {
		case "kill":
			err = sim.ChaosKill(*state, node)
		case "partition", "heal":
			err = sim.ChaosNet(*state, action, node, 0)
		case "slow":
			err = sim.ChaosNet(*state, action, node, *delayMs)
			out["delay_ms"] = *delayMs
		default:
			fmt.Fprint(stderr, usage)
			return 2
		}
		result = out
	case "_dc", "_dc-init":
		if *sockets == "" {
			fmt.Fprintln(stderr, "anden: --sockets is required")
			return 2
		}
		abs, aerr := filepath.Abs(*state)
		if aerr != nil {
			fmt.Fprintln(stderr, "anden:", aerr)
			return 1
		}
		if cmd == "_dc" {
			err = sim.RunDC(abs, *sockets, stderr)
		} else {
			err = sim.RunDCInit(abs, *sockets, stderr)
		}
		if err != nil {
			fmt.Fprintf(stderr, "anden %s: %v\n", cmd, err)
			return 1
		}
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "anden:", err)
		return 1
	}
	if *jsonOut {
		out, merr := json.Marshal(result)
		if merr != nil {
			fmt.Fprintln(stderr, "anden:", merr)
			return 1
		}
		fmt.Fprintln(stdout, string(out))
	}
	return 0
}

type nodeStatus struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Epoch uint64 `json:"epoch"`
}

// runStatus prints the nodes and services that the API reports.
func runStatus(state string, timeout time.Duration, jsonOut bool, stdout, stderr io.Writer) int {
	socket, err := apiSocket(state)
	if err != nil {
		fmt.Fprintln(stderr, "anden:", err)
		return 1
	}
	conn, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Fprintln(stderr, "anden:", err)
		return 1
	}
	defer conn.Close()
	client := andenv1.NewAndenClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	nodes, err := client.ListNodes(ctx, &andenv1.ListNodesRequest{})
	if err != nil {
		fmt.Fprintln(stderr, "anden:", err)
		return 1
	}
	services, err := client.ListServices(ctx, &andenv1.ListServicesRequest{})
	if err != nil {
		fmt.Fprintln(stderr, "anden:", err)
		return 1
	}
	if !jsonOut {
		for _, n := range nodes.GetNodes() {
			fmt.Fprintf(stdout, "%s\t%s\tepoch %d\n", n.GetName(), n.GetState(), n.GetEpoch())
		}
		for _, s := range services.GetServices() {
			fmt.Fprintf(stdout, "%s\t%s\tv%d\t%s\t%s\n", s.GetSpec().GetName(), s.GetState(), s.GetVersion(), s.GetNode(), s.GetAddr())
		}
		return 0
	}
	out := struct {
		Nodes    []nodeStatus      `json:"nodes"`
		Services []json.RawMessage `json:"services"`
	}{Nodes: []nodeStatus{}, Services: []json.RawMessage{}}
	for _, n := range nodes.GetNodes() {
		out.Nodes = append(out.Nodes, nodeStatus{Name: n.GetName(), State: n.GetState().String(), Epoch: n.GetEpoch()})
	}
	marshal := protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}
	for _, s := range services.GetServices() {
		raw, err := marshal.Marshal(s)
		if err != nil {
			fmt.Fprintln(stderr, "anden:", err)
			return 1
		}
		out.Services = append(out.Services, raw)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		fmt.Fprintln(stderr, "anden:", err)
		return 1
	}
	fmt.Fprintln(stdout, string(raw))
	return 0
}
