// Package agent is the node agent of Andén: it registers with the control
// plane, renews its lease every 500 ms, fences itself when the lease runs out
// (D08) and makes the instances of its node match what the control plane asks
// for in each heartbeat response.
package agent

import (
	"context"
	"encoding/json"
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
	"github.com/CesarManzoCode/anden/internal/wake"
)

const (
	defaultHeartbeat = 500 * time.Millisecond
	rpcTimeout       = 2 * time.Second
	// reconcileInterval is how often the agent compares what it has with what
	// was asked and looks for instances that died.
	reconcileInterval = 100 * time.Millisecond
	cgroupRoot        = "/sys/fs/cgroup"
	// sleepRetry is how long to wait after a sleep aborted by a live socket
	// before an order to sleep tries again.
	sleepRetry = time.Second
	// placementTimeout is how long a wake waits for the control plane before
	// it goes on with this node.
	placementTimeout = 200 * time.Millisecond
	// wakeEventsFile, in the state of the node, has one line per wake
	// (contract, section 5).
	wakeEventsFile = "wake_events.jsonl"
	// transitionsFile, in the state of the node, has the counters of the
	// machine of each service, rewritten whole at each change.
	transitionsFile = "transitions.json"
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
	nft     *wake.NFT
	proxy   *wake.Proxy
	// beatNow asks for a heartbeat without waiting for the tick.
	beatNow chan struct{}
	// lease is the lease of D08: without it nothing starts or wakes.
	lease *lease
	// imageMu serializes transfers and overlay mounts, because a mount extracts
	// the layers it lacks: two instances of one image would extract them twice.
	imageMu sync.Mutex
	// eventsMu keeps the lines of wake_events.jsonl whole.
	eventsMu sync.Mutex
	// statsMu guards stats, the counters of each service by name, and the
	// writes of transitions.json.
	statsMu sync.Mutex
	stats   map[string]transitionStats

	mu sync.Mutex
	// control and epoch are the registration, for what does not wait for a
	// heartbeat: epoch is 0 while there is none.
	control   andenv1.AgentControlClient
	epoch     uint64
	desired   []*andenv1.Assignment
	instances map[string]*instance
	// intents are what this node decided and the control plane does not know
	// yet, by instance id. They win over desired until it does.
	intents  map[string]*intent
	blobsURL string
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
		nft:       wake.NewNFT(),
		proxy:     wake.NewProxy(),
		beatNow:   make(chan struct{}, 1),
		lease:     newLease(time.Now),
		instances: map[string]*instance{},
		intents:   map[string]*intent{},
	}, nil
}

// Run keeps the heartbeat, the reconcile loop and the watch of the lease going
// until ctx ends. If the control plane is gone the instances keep running for
// the rest of the lease, and the fence ends them when it runs out.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.nft.Setup(); err != nil {
		return err
	}
	ln, err := wake.Listen(ctx)
	if err != nil {
		return err
	}
	go func() {
		if err := a.proxy.Serve(ctx, ln); err != nil {
			log.Printf("wake proxy: %v", err)
		}
	}()
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
	go a.fenceLoop(ctx)

	s := &session{client: andenv1.NewAgentControlClient(conn)}
	a.mu.Lock()
	a.control = s.client
	a.mu.Unlock()
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
		case <-a.beatNow:
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
	// A fenced node comes back only with a new epoch (D08).
	if !a.lease.held() {
		s.epoch = 0
	}
	if s.epoch == 0 {
		asked := time.Now()
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
		a.epoch = s.epoch
		a.mu.Unlock()
		a.lease.grant(asked, msDuration(resp.GetLeaseTtlMs()))
		log.Printf("registered as %s, epoch %d", a.cfg.Node, s.epoch)
	}
	// Intents go first, so a report of what they caused finds them applied.
	settled, err := a.sendIntents(ctx, s)
	if err != nil {
		return a.refused(s, err)
	}
	asked := time.Now()
	resp, err := s.client.Heartbeat(ctx, &andenv1.HeartbeatRequest{Node: a.cfg.Node, Epoch: s.epoch})
	if err != nil {
		return a.refused(s, err)
	}
	a.lease.renew(asked, msDuration(resp.GetLeaseTtlMs()))
	a.mu.Lock()
	// The answer to a heartbeat sent before a fence is not followed: the node
	// waits for the one after its new registration.
	if a.lease.held() {
		a.desired = resp.GetAssignments()
	}
	for _, sent := range settled {
		if a.intents[sent.id] == sent {
			delete(a.intents, sent.id)
		}
	}
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
		a.mu.Lock()
		a.epoch = 0
		a.mu.Unlock()
	}
	return err
}

// place is the placement of a wake: it tells the control plane that the
// instance wakes and waits for its answer at most placementTimeout. With an
// answer or without one the wake stays on this node, because the answer does
// not choose a node yet (D07, K4). The intent from declare stays as well; the
// control plane takes the same intent twice without change.
func (a *Agent) place(in *instance) {
	a.mu.Lock()
	client, epoch := a.control, a.epoch
	a.mu.Unlock()
	if client == nil || epoch == 0 {
		log.Printf("instance %s: placement: not registered, waking on this node", in.id)
		return
	}
	ctx, cancel := context.WithTimeout(in.ctx, placementTimeout)
	defer cancel()
	_, err := client.SetAwake(ctx, &andenv1.SetAwakeRequest{Node: a.cfg.Node, Epoch: epoch, Service: in.service,
		Generation: in.generation, Awake: true})
	if err != nil {
		log.Printf("instance %s: placement: no answer, waking on this node: %v", in.id, err)
	}
}

// recordWake appends one event to wake_events.jsonl.
func (a *Agent) recordWake(ev wake.Event) error {
	line, err := ev.Line()
	if err != nil {
		return err
	}
	a.eventsMu.Lock()
	defer a.eventsMu.Unlock()
	f, err := os.OpenFile(filepath.Join(a.cfg.State, wakeEventsFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(line)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// transitionStats is the entry of one service in transitions.json.
type transitionStats struct {
	State  string `json:"state"`
	Sleeps uint64 `json:"sleeps"`
	Held   uint64 `json:"held"`
}

// observe keeps the counters of a service and rewrites transitions.json. The
// file is replaced by a rename, so a reader never finds half of it. A new
// instance of the service starts its counters again.
func (a *Agent) observe(service string, st wake.Stats) error {
	a.statsMu.Lock()
	defer a.statsMu.Unlock()
	if a.stats == nil {
		a.stats = map[string]transitionStats{}
	}
	a.stats[service] = transitionStats{State: st.State.String(), Sleeps: st.Sleeps, Held: st.Held}
	raw, err := json.Marshal(a.stats)
	if err != nil {
		return err
	}
	path := filepath.Join(a.cfg.State, transitionsFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// snapshot returns what to report, and the value of changes it corresponds to.
func (a *Agent) snapshot() (uint64, []*andenv1.InstanceReport) {
	a.mu.Lock()
	defer a.mu.Unlock()
	reports := make([]*andenv1.InstanceReport, 0, len(a.instances))
	for _, in := range a.instances {
		// Asleep there is no instance to report: the node only keeps its place.
		if in.state == andenv1.InstanceState_INSTANCE_STATE_STOPPED {
			continue
		}
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
		a.watchIdle(time.Now())
	}
}

// reconcile starts, stops, sleeps and wakes what the comparison says. Each action
// runs on its own goroutine: an image transfer must not delay anything else.
// Without a valid lease it does nothing: the fence takes care of the node.
func (a *Agent) reconcile(ctx context.Context) {
	if !a.lease.valid() {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	desired := a.effective()
	have := make(map[string]local, len(a.instances))
	for id, in := range a.instances {
		have[id] = local{service: in.service, addr: in.addr, stopping: in.stopping}
	}
	awake := make(map[string]bool, len(desired))
	for _, as := range desired {
		awake[as.GetInstanceId()] = as.GetAwake()
		if in := a.instances[as.GetInstanceId()]; in != nil {
			follow(in, as.GetSpec(), time.Now())
		}
		// Only from Asleep: a sleep under way that has not told anyone yet
		// must not read as a wake.
		if in := a.instances[as.GetInstanceId()]; in != nil && as.GetAwake() && !in.stopping && in.machine.State() == wake.Asleep {
			in.machine.Wake()
		}
	}
	start, stop := diff(desired, have)
	for _, id := range stop {
		in := a.instances[id]
		isAwake, assigned := awake[id]
		switch sleepAction(assigned && !isAwake, in.machine.State(), in.state) {
		case actionSleep:
			if now := time.Now(); !now.Before(in.sleepAfter) {
				in.sleepAfter = now.Add(sleepRetry)
				go a.sleep(in)
			}
		case actionStop:
			in.stopping = true
			go a.stop(in)
		}
	}
	for _, as := range start {
		in, err := a.newInstance(ctx, as, false)
		if err != nil {
			log.Printf("instance %s: %v", as.GetInstanceId(), err)
			continue
		}
		a.instances[in.id] = in
		a.proxy.Register(in.machine)
		a.changes++
		go a.start(in, as)
	}
	for _, as := range dormant(desired, have) {
		in, err := a.newInstance(ctx, as, true)
		if err != nil {
			log.Printf("instance %s: %v", as.GetInstanceId(), err)
			continue
		}
		a.instances[in.id] = in
		a.proxy.Register(in.machine)
		go func() {
			if err := in.machine.Settle(); err != nil {
				log.Printf("instance %s: %v", in.id, err)
			}
		}()
	}
}

// intent is a change of desired state this node made on its own: a connection
// woke a service or its idle window ended. sent says the control plane has it.
type intent struct {
	id         string
	service    string
	generation uint64
	awake      bool
	sent       bool
}

// effective is desired with the intents the control plane has not reflected yet.
func (a *Agent) effective() []*andenv1.Assignment {
	out := make([]*andenv1.Assignment, 0, len(a.desired))
	for _, as := range a.desired {
		if it := a.intents[as.GetInstanceId()]; it != nil && it.awake != as.GetAwake() {
			as = &andenv1.Assignment{InstanceId: as.GetInstanceId(), Spec: as.GetSpec(), Version: as.GetVersion(),
				Generation: as.GetGeneration(), Addr: as.GetAddr(), Awake: it.awake}
		}
		out = append(out, as)
	}
	return out
}

// declare records that in should be awake or not, if that is news.
func (a *Agent) declare(in *instance, awake bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.intents[in.id] == nil {
		for _, as := range a.desired {
			if as.GetInstanceId() == in.id && as.GetAwake() == awake {
				return
			}
		}
	}
	a.intents[in.id] = &intent{id: in.id, service: in.service, generation: in.generation, awake: awake}
	select {
	case a.beatNow <- struct{}{}:
	default:
	}
}

// sendIntents tells the control plane what the node decided. It returns the ones
// it already has: a heartbeat asked for after this reflects them, so they can go.
func (a *Agent) sendIntents(ctx context.Context, s *session) ([]*intent, error) {
	a.mu.Lock()
	pending := make([]*intent, 0, len(a.intents))
	for _, it := range a.intents {
		pending = append(pending, it)
	}
	a.mu.Unlock()
	for _, it := range pending {
		if it.sent {
			continue
		}
		_, err := s.client.SetAwake(ctx, &andenv1.SetAwakeRequest{Node: a.cfg.Node, Epoch: s.epoch, Service: it.service,
			Generation: it.generation, Awake: it.awake})
		if err != nil {
			return nil, err
		}
		it.sent = true
	}
	return pending, nil
}

// sleep takes an instance to sleep. It is the only way there: an order from the
// control plane and the idle signal both end here.
// A connection that came before the processes stopped aborts the sleep, and the
// service stays awake as if that connection had woken it.
func (a *Agent) sleep(in *instance) {
	err := in.machine.Sleep()
	if err == nil || errors.Is(err, wake.ErrNotActive) {
		return
	}
	switch {
	case errors.Is(err, wake.ErrWoken):
		a.declare(in, true)
	case !errors.Is(err, wake.ErrBusy):
		log.Printf("instance %s: sleep: %v", in.id, err)
	}
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	in.sleepAfter = now.Add(sleepRetry)
	if in.idle != nil {
		in.idle.Touch(now)
	}
}

// watchIdle samples each running instance at a quarter of its window and takes
// to sleep the ones that have spent the whole window inactive (D05).
func (a *Agent) watchIdle(now time.Time) {
	a.mu.Lock()
	due := make([]*instance, 0, len(a.instances))
	for _, in := range a.instances {
		if in.idle != nil && in.state == andenv1.InstanceState_INSTANCE_STATE_RUNNING && !in.stopping &&
			!now.Before(in.nextSample) && in.machine.State() == wake.Active {
			in.nextSample = now.Add(in.idle.Interval())
			due = append(due, in)
		}
	}
	a.mu.Unlock()
	for _, in := range due {
		sockets, tx, err := readActivity(in.paths.cgroup)
		if err != nil {
			log.Printf("instance %s: %v", in.id, err)
			continue
		}
		a.mu.Lock()
		idle := in.idle.Observe(now, sockets, tx)
		a.mu.Unlock()
		if idle {
			go a.sleep(in)
		}
	}
}

// watchInstances marks as failed each running instance whose cgroup emptied.
func (a *Agent) watchInstances() {
	a.mu.Lock()
	running := make([]*instance, 0, len(a.instances))
	for _, in := range a.instances {
		// Outside Active the processes are expected to come and go.
		if in.state == andenv1.InstanceState_INSTANCE_STATE_RUNNING && !in.stopping && in.machine.State() == wake.Active {
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
