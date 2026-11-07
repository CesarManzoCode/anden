// Package agent is the node agent of Andén: it registers with the control
// plane, renews its lease every 500 ms and makes the instances of its node
// match what the control plane asks for in each heartbeat response.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	andenv1 "github.com/CesarManzoCode/anden/gen/anden/v1"
	"github.com/CesarManzoCode/anden/internal/image"
)

const (
	defaultHeartbeat = 500 * time.Millisecond
	rpcTimeout       = 2 * time.Second
	// reconcileInterval is how often the agent compares what it has with what
	// was asked and looks for instances that died.
	reconcileInterval = 100 * time.Millisecond
	cgroupRoot        = "/sys/fs/cgroup"
)

// Config is the command line of the agent.
type Config struct {
	Node      string
	Addr      string
	Control   string
	State     string
	Cgroup    string
	Subnet    string
	Runtime   string
	CPUMillis uint32
	MemoryMiB uint32
}

type Agent struct {
	cfg     Config
	store   *image.Store
	gateway string
	bits    int
	dns     string
	client  *http.Client
	// imageMu serializes transfers and overlay mounts, because a mount extracts
	// the layers it lacks: two instances of one image would extract them twice.
	imageMu sync.Mutex

	mu        sync.Mutex
	desired   []*andenv1.Assignment
	instances map[string]*instance
	blobsURL  string
	// changes counts what happened to instances, to know when to report.
	changes uint64
}

// New prepares the node's directories. It does not contact the control plane.
func New(cfg Config) (*Agent, error) {
	prefix, err := netip.ParsePrefix(cfg.Subnet)
	if err != nil {
		return nil, fmt.Errorf("--subnet: %w", err)
	}
	dns, _, err := net.SplitHostPort(cfg.Control)
	if err != nil {
		return nil, fmt.Errorf("--control: %w", err)
	}
	if !strings.HasPrefix(cfg.Cgroup, cgroupRoot+"/") {
		return nil, fmt.Errorf("--cgroup %s is not under %s", cfg.Cgroup, cgroupRoot)
	}
	st, err := image.Open(filepath.Join(cfg.State, "store"))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(cfg.State, "instances"), 0o755); err != nil {
		return nil, err
	}
	return &Agent{
		cfg:       cfg,
		store:     st,
		gateway:   prefix.Masked().Addr().Next().String(),
		bits:      prefix.Bits(),
		dns:       dns,
		client:    &http.Client{Transport: &http.Transport{DisableKeepAlives: true}},
		instances: map[string]*instance{},
	}, nil
}

// Run keeps the heartbeat and the reconcile loop going until ctx ends. If the
// control plane is gone the instances keep running as they are.
func (a *Agent) Run(ctx context.Context) error {
	// The short backoff lets the agent find a restarted andend within the 4 s
	// after which it would count the node as dead.
	conn, err := grpc.NewClient("passthrough:///"+a.cfg.Control,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithNoProxy(),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff:           backoff.Config{BaseDelay: 100 * time.Millisecond, Multiplier: 1.6, Jitter: 0.2, MaxDelay: 500 * time.Millisecond},
			MinConnectTimeout: rpcTimeout,
		}))
	if err != nil {
		return err
	}
	defer conn.Close()
	go a.reconcileLoop(ctx)

	s := &session{client: andenv1.NewAgentControlClient(conn)}
	tick := time.NewTicker(defaultHeartbeat)
	defer tick.Stop()
	for {
		if err := a.beat(ctx, s); err != nil && ctx.Err() == nil {
			log.Printf("heartbeat: %v", err)
		}
		if s.interval > 0 {
			tick.Reset(s.interval)
			s.interval = 0
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// session is the agent's view of its registration.
type session struct {
	client andenv1.AgentControlClient
	// epoch is 0 until registered, and again when the control plane refuses it.
	epoch    uint64
	interval time.Duration
	// reported is the value of changes that the control plane has, plus one so
	// that its zero value means nothing was reported yet.
	reported uint64
}

func (a *Agent) beat(ctx context.Context, s *session) error {
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	if s.epoch == 0 {
		resp, err := s.client.Register(ctx, &andenv1.RegisterRequest{
			Node: a.cfg.Node, Addr: a.cfg.Addr, CpuMillis: a.cfg.CPUMillis, MemoryMib: a.cfg.MemoryMiB, Subnet: a.cfg.Subnet,
		})
		if err != nil {
			return fmt.Errorf("register: %w", err)
		}
		s.epoch, s.reported = resp.GetEpoch(), 0
		s.interval = time.Duration(resp.GetHeartbeatIntervalMs()) * time.Millisecond
		a.mu.Lock()
		a.blobsURL = resp.GetBlobsUrl()
		a.mu.Unlock()
		log.Printf("registered as %s, epoch %d", a.cfg.Node, s.epoch)
	}
	resp, err := s.client.Heartbeat(ctx, &andenv1.HeartbeatRequest{Node: a.cfg.Node, Epoch: s.epoch})
	if err != nil {
		return a.refused(s, err)
	}
	a.mu.Lock()
	a.desired = resp.GetAssignments()
	a.mu.Unlock()

	changes, reports := a.snapshot()
	if s.reported == changes+1 {
		return nil
	}
	_, err = s.client.ReportInstances(ctx, &andenv1.ReportInstancesRequest{Node: a.cfg.Node, Epoch: s.epoch, Instances: reports})
	if err != nil {
		return a.refused(s, err)
	}
	s.reported = changes + 1
	return nil
}

// refused drops the registration when the control plane says the epoch is not
// valid; any other error only waits for the next beat.
func (a *Agent) refused(s *session, err error) error {
	switch status.Code(err) {
	case codes.FailedPrecondition, codes.NotFound:
		s.epoch = 0
	}
	return err
}

// snapshot returns what to report, and the value of changes it corresponds to.
func (a *Agent) snapshot() (uint64, []*andenv1.InstanceReport) {
	a.mu.Lock()
	defer a.mu.Unlock()
	reports := make([]*andenv1.InstanceReport, 0, len(a.instances))
	for _, in := range a.instances {
		reports = append(reports, &andenv1.InstanceReport{
			Id: in.id, Service: in.service, Version: in.version, Generation: in.generation,
			State: in.state, Addr: in.addr, Seccomp: in.seccomp, Route: in.route,
		})
	}
	return a.changes, reports
}

func (a *Agent) reconcileLoop(ctx context.Context) {
	tick := time.NewTicker(reconcileInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		a.reconcile(ctx)
		a.watchInstances()
	}
}

// reconcile starts and stops what diff says. Each action runs on its own
// goroutine: an image transfer must not delay anything else.
func (a *Agent) reconcile(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	have := make(map[string]local, len(a.instances))
	for id, in := range a.instances {
		have[id] = local{service: in.service, stopping: in.stopping}
	}
	start, stop := diff(a.desired, have)
	for _, id := range stop {
		in := a.instances[id]
		in.stopping = true
		go a.stop(in)
	}
	for _, as := range start {
		in := a.newInstance(ctx, as)
		a.instances[in.id] = in
		a.changes++
		go a.start(in, as)
	}
}

// watchInstances marks as failed each running instance whose cgroup emptied.
func (a *Agent) watchInstances() {
	a.mu.Lock()
	running := make([]*instance, 0, len(a.instances))
	for _, in := range a.instances {
		if in.state == andenv1.InstanceState_INSTANCE_STATE_RUNNING && !in.stopping {
			running = append(running, in)
		}
	}
	a.mu.Unlock()
	for _, in := range running {
		populated, err := cgroupPopulated(in.paths.cgroup)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("instance %s: %v", in.id, err)
			continue
		}
		if !populated {
			log.Printf("instance %s: no process left", in.id)
			a.setState(in, andenv1.InstanceState_INSTANCE_STATE_FAILED)
		}
	}
}

func (a *Agent) setState(in *instance, state andenv1.InstanceState) {
	a.mu.Lock()
	defer a.mu.Unlock()
	in.state = state
	a.changes++
}
