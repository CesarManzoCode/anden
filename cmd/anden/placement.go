package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/CesarManzoCode/anden/internal/scheduler"
)

// runPlacement is `anden placement solve`: the scheduler of D07 over an
// instance read from a file, without a datacenter.
func runPlacement(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "solve" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	flags := flag.NewFlagSet("anden placement solve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	input := flags.String("input", "", `instance as JSON: {"nodes": [...], "services": [...]}`)
	flags.String("state", defaultState, "state directory; unused, the scheduler needs no datacenter")
	jsonOut := flags.Bool("json", false, "print a JSON object on stdout")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *input == "" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	raw, err := os.ReadFile(*input)
	if err != nil {
		fmt.Fprintln(stderr, "anden:", err)
		return 1
	}
	var problem scheduler.Problem
	if err := json.Unmarshal(raw, &problem); err != nil {
		fmt.Fprintln(stderr, "anden: instance:", err)
		return 1
	}
	if err := scheduler.Check(problem); err != nil {
		fmt.Fprintln(stderr, "anden: instance:", err)
		return 1
	}
	placement := scheduler.Solve(problem)
	if *jsonOut {
		out, err := json.Marshal(map[string]scheduler.Placement{"placement": placement})
		if err != nil {
			fmt.Fprintln(stderr, "anden:", err)
			return 1
		}
		fmt.Fprintln(stdout, string(out))
		return 0
	}
	names := make([]string, 0, len(placement))
	for name := range placement {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		node := placement[name]
		if node == "" {
			node = "-"
		}
		fmt.Fprintf(stdout, "%s\t%s\n", name, node)
	}
	return 0
}
