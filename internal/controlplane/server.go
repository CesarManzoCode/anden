package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"google.golang.org/grpc"

	andenv1 "github.com/CesarManzoCode/anden/gen/anden/v1"
	"github.com/CesarManzoCode/anden/internal/dns"
	"github.com/CesarManzoCode/anden/internal/metrics"
	"github.com/CesarManzoCode/anden/internal/store"
)

// Options are the optional parts of andend. Empty fields disable them.
type Options struct {
	// Registry is the central image store whose blobs agents fetch.
	Registry string
	// BlobsListen is the TCP address of the blob server.
	BlobsListen string
	// EdgeIP is the address of the edge of D21.
	EdgeIP string
	// DNSListen is the UDP and TCP address of the DNS of D22.
	DNSListen string
	// MetricsListen is the TCP address of the aggregated /metrics.
	MetricsListen string
	// AgentMetricsPort is the port of /metrics on the address of each agent.
	AgentMetricsPort int
}

type ControlPlane struct {
	store *store.Store
	rec   *Reconciler
	live  *liveness
	opts  Options
}

func New(st *store.Store, opts Options) *ControlPlane {
	c := &ControlPlane{store: st, rec: NewReconciler(st), live: newLiveness(time.Now), opts: opts}
	c.rec.ready = c.live.ready
	if opts.EdgeIP != "" {
		c.rec.edge = NewEdge(opts.EdgeIP)
	}
	if opts.DNSListen != "" {
		c.rec.records = dns.NewRecords()
	}
	return c
}

func (c *ControlPlane) blobsURL() string {
	if c.opts.BlobsListen == "" {
		return ""
	}
	return "http://" + c.opts.BlobsListen
}

// metricsNodes are the registered nodes for the aggregate: the /metrics of
// each agent and whether the failure detector counts it alive.
func (c *ControlPlane) metricsNodes(ctx context.Context) ([]metrics.Node, error) {
	ready := c.live.snapshot()
	nodes, err := c.store.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]metrics.Node, 0, len(nodes))
	for _, n := range nodes {
		url := "http://" + net.JoinHostPort(n.Addr, strconv.Itoa(c.opts.AgentMetricsPort)) + "/metrics"
		out = append(out, metrics.Node{Name: n.Name, URL: url, Up: ready(n.Name)})
	}
	return out, nil
}

var blobHexRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// blobHandler serves GET /blobs/sha256/<hex> from a store. The agent verifies
// each blob against its digest as it receives it.
func blobHandler(registry string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /blobs/sha256/{hex}", func(w http.ResponseWriter, r *http.Request) {
		hex := r.PathValue("hex")
		if !blobHexRE.MatchString(hex) {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(registry, "blobs", "sha256", hex))
	})
	return mux
}

// Serve answers gRPC on the Unix socket for host clients and, when tcpAddr is
// not empty, on TCP for agents (D23), serves blobs if configured, and runs the
// reconciler until ctx ends.
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
	if c.opts.Registry != "" && c.opts.BlobsListen != "" {
		bl, err := net.Listen("tcp", c.opts.BlobsListen)
		if err != nil {
			for _, l := range listeners {
				l.Close()
			}
			return err
		}
		blobs := &http.Server{Handler: blobHandler(c.opts.Registry)}
		defer blobs.Close()
		go func() {
			if err := blobs.Serve(bl); !errors.Is(err, http.ErrServerClosed) {
				log.Printf("blob server: %v", err)
			}
		}()
	}

	if c.opts.MetricsListen != "" {
		ml, err := net.Listen("tcp", c.opts.MetricsListen)
		if err != nil {
			for _, l := range listeners {
				l.Close()
			}
			return fmt.Errorf("metrics: %w", err)
		}
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", &metrics.Aggregator{Nodes: c.metricsNodes,
			Client: &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}})
		msrv := &http.Server{Handler: mux}
		defer msrv.Close()
		go func() {
			if err := msrv.Serve(ml); !errors.Is(err, http.ErrServerClosed) {
				log.Printf("metrics: %v", err)
			}
		}()
	}

	if c.opts.DNSListen != "" {
		names, err := dns.Listen(c.opts.DNSListen, c.rec.records)
		if err != nil {
			for _, l := range listeners {
				l.Close()
			}
			return fmt.Errorf("dns: %w", err)
		}
		dnsCtx, stopDNS := context.WithCancel(ctx)
		dnsDone := make(chan struct{})
		defer func() {
			stopDNS()
			<-dnsDone
		}()
		go func() {
			defer close(dnsDone)
			if err := names.Serve(dnsCtx); err != nil {
				log.Printf("dns: %v", err)
			}
		}()
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
