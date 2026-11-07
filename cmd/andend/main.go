// Command andend is Andén's control plane: gRPC API on a Unix socket and on
// TCP, state in PostgreSQL over a Unix socket, the level reconciler, the blob
// server for agents and the edge.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/CesarManzoCode/anden/internal/controlplane"
	"github.com/CesarManzoCode/anden/internal/store"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("andend", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiSocket := flags.String("api-socket", "", "Unix socket for host clients (required)")
	listen := flags.String("listen", "10.80.0.1:7443", "TCP address for agents; empty disables it")
	pgSocketDir := flags.String("pg-socket-dir", "", "directory of the PostgreSQL Unix socket (required)")
	pgUser := flags.String("pg-user", "postgres", "PostgreSQL user")
	pgDatabase := flags.String("pg-database", "postgres", "PostgreSQL database")
	connectTimeout := flags.Duration("pg-connect-timeout", 30*time.Second, "how long to retry the first connection")
	interval := flags.Duration("reconcile-interval", 200*time.Millisecond, "time between reconciler passes")
	registry := flags.String("registry", "", "central image store to serve blobs from; empty disables the blob server")
	blobsListen := flags.String("blobs-listen", "", "TCP address of the blob server")
	edgeIP := flags.String("edge-ip", "", "edge address to program in nftables; empty disables the edge")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *apiSocket == "" || *pgSocketDir == "" || flags.NArg() != 0 {
		flags.Usage()
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := openStore(ctx, store.ConnString(*pgSocketDir, *pgUser, *pgDatabase), *connectTimeout)
	if err != nil {
		fmt.Fprintln(stderr, "andend: postgres:", err)
		return 1
	}
	defer st.Close()

	if err := controlplane.New(st, controlplane.Options{Registry: *registry, BlobsListen: *blobsListen, EdgeIP: *edgeIP}).Serve(ctx, *apiSocket, *listen, *interval); err != nil {
		fmt.Fprintln(stderr, "andend:", err)
		return 1
	}
	return 0
}

// openStore retries while Postgres starts, which happens alongside andend.
func openStore(ctx context.Context, conn string, timeout time.Duration) (*store.Store, error) {
	deadline := time.Now().Add(timeout)
	for {
		st, err := store.Open(ctx, conn)
		if err == nil || time.Now().After(deadline) || ctx.Err() != nil {
			return st, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
