// Command anden-agent is Andén's node agent: it registers with andend, renews
// its lease with a heartbeat and runs the instances placed on its node.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/CesarManzoCode/anden/internal/agent"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("anden-agent", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var cfg agent.Config
	flags.StringVar(&cfg.Node, "node", "", "node name (required)")
	flags.StringVar(&cfg.Addr, "addr", "", "address of the node (required)")
	flags.StringVar(&cfg.Control, "control", "", "gRPC address of andend, host:port (required)")
	flags.StringVar(&cfg.State, "state", "", "disk directory of the node (required)")
	flags.StringVar(&cfg.Cgroup, "cgroup", "", "cgroup directory of the node, under /sys/fs/cgroup (required)")
	flags.StringVar(&cfg.Subnet, "subnet", "", "subnet of the svc0 bridge (required)")
	flags.StringVar(&cfg.Runtime, "runtime", "", "path of anden-runtime (required)")
	cpuMillis := flags.Uint("cpu-millis", 0, "CPU the node offers, in thousandths of a core (required)")
	memoryMiB := flags.Uint("memory-mib", 0, "memory the node offers, in MiB (required)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || cfg.Node == "" || cfg.Addr == "" || cfg.Control == "" || cfg.State == "" ||
		cfg.Cgroup == "" || cfg.Subnet == "" || cfg.Runtime == "" || *cpuMillis == 0 || *memoryMiB == 0 {
		flags.Usage()
		return 2
	}
	cfg.CPUMillis, cfg.MemoryMiB = uint32(*cpuMillis), uint32(*memoryMiB)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	a, err := agent.New(cfg)
	if err != nil {
		fmt.Fprintln(stderr, "anden-agent:", err)
		return 1
	}
	if err := a.Run(ctx); err != nil {
		fmt.Fprintln(stderr, "anden-agent:", err)
		return 1
	}
	return 0
}
