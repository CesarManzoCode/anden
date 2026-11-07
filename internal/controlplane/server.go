package controlplane

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc"

	andenv1 "github.com/CesarManzoCode/anden/gen/anden/v1"
	"github.com/CesarManzoCode/anden/internal/store"
)

type ControlPlane struct {
	store *store.Store
	rec   *Reconciler
	live  *liveness
}

func New(st *store.Store) *ControlPlane {
	return &ControlPlane{store: st, rec: NewReconciler(st), live: newLiveness()}
}

// Serve answers gRPC on the Unix socket for host clients and, when tcpAddr is
// not empty, on TCP for agents (D23), and runs the reconciler until ctx ends.
func (c *ControlPlane) Serve(ctx context.Context, unixPath, tcpAddr string, interval time.Duration) error {
	if err := os.MkdirAll(filepath.Dir(unixPath), 0o700); err != nil {
		return err
	}
	if err := os.Remove(unixPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listeners := []net.Listener{}
	ul, err := net.Listen("unix", unixPath)
	if err != nil {
		return err
	}
	listeners = append(listeners, ul)
	if tcpAddr != "" {
		tl, err := net.Listen("tcp", tcpAddr)
		if err != nil {
			ul.Close()
			return err
		}
		listeners = append(listeners, tl)
	}

	srv := grpc.NewServer()
	andenv1.RegisterAndenServer(srv, &andenService{cp: c})
	andenv1.RegisterAgentControlServer(srv, &agentService{cp: c})

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	recDone := make(chan struct{})
	go func() {
		c.rec.Run(ctx, interval)
		close(recDone)
	}()
	errc := make(chan error, len(listeners))
	for _, l := range listeners {
		go func() { errc <- srv.Serve(l) }()
	}
	select {
	case <-ctx.Done():
		srv.GracefulStop()
		err = nil
	case err = <-errc:
		srv.Stop()
	}
	cancel()
	<-recDone
	return err
}
