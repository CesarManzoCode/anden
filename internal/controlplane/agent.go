package controlplane

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	andenv1 "github.com/CesarManzoCode/anden/gen/anden/v1"
	"github.com/CesarManzoCode/anden/internal/store"
)

// Lease timing of D08: the agent renews every 500 ms with a 2 s TTL, and the
// control plane declares the node dead after 2×TTL.
const (
	leaseTTL          = 2 * time.Second
	heartbeatInterval = 500 * time.Millisecond
	deadAfter         = 2 * leaseTTL
)

// liveness is the failure detector of D08: it tracks when each node was last
// heard from, with the monotonic clock, and a node not heard from in 2×TTL is
// dead. Its agent stopped renewing its lease at least 2×TTL ago, so it fenced
// itself a TTL before. A node not heard from since andend started counts from
// the start, so a restart of the control plane does not declare every node dead.
type liveness struct {
	mu      sync.Mutex
	now     func() time.Time
	started time.Time
	seen    map[string]time.Time
}

func newLiveness(now func() time.Time) *liveness {
	return &liveness{now: now, started: now(), seen: map[string]time.Time{}}
}

func (l *liveness) touch(node string) {
	l.mu.Lock()
	l.seen[node] = l.now()
	l.mu.Unlock()
}

func (l *liveness) ready(node string) bool {
	return l.snapshot()(node)
}

// snapshot is ready as it stands now, for a decision that reads the store
// afterwards: a node it finds alive had registered, and the store had dropped
// its old reports, before that read.
func (l *liveness) snapshot() func(node string) bool {
	l.mu.Lock()
	seen, started, now := maps.Clone(l.seen), l.started, l.now()
	l.mu.Unlock()
	return func(node string) bool {
		last, ok := seen[node]
		if !ok {
			last = started
		}
		return now.Sub(last) < deadAfter
	}
}

// checkEpoch refuses what a node sends with an epoch that is not its current
// one: it comes from before the node registered again, as what a fenced agent
// sent before its new registration (D08).
func checkEpoch(node string, current int64, got uint64) error {
	if uint64(current) != got {
		return status.Errorf(codes.FailedPrecondition, "epoch %d of %s is stale, current is %d", got, node, current)
	}
	return nil
}

// holdSingletons drops from what node receives each singleton of which a node
// that is not dead still reports an instance of another generation, running or
// starting: a singleton becomes active on its new node only once the previous
// one has let it go or has been declared dead (D08).
func holdSingletons(node string, assignments []*andenv1.Assignment, instances []store.Instance, ready func(string) bool) []*andenv1.Assignment {
	out := make([]*andenv1.Assignment, 0, len(assignments))
	for _, as := range assignments {
		held := as.GetSpec().GetSingleton() && slices.ContainsFunc(instances, func(in store.Instance) bool {
			return in.Service == as.GetSpec().GetName() && in.Node != node && in.Generation != int64(as.GetGeneration()) &&
				(in.State == instRunning || in.State == instStarting) && ready(in.Node)
		})
		if !held {
			out = append(out, as)
		}
	}
	return out
}

type agentService struct {
	andenv1.UnimplementedAgentControlServer
	cp *ControlPlane
}

// Register gives the node a new epoch; reports with an older one are refused,
// and what it reported under the last one is dropped.
func (a *agentService) Register(ctx context.Context, req *andenv1.RegisterRequest) (*andenv1.RegisterResponse, error) {
	if req.GetNode() == "" {
		return nil, status.Error(codes.InvalidArgument, "node is required")
	}
	epoch, err := a.cp.store.RegisterNode(ctx, store.Node{
		Name:      req.GetNode(),
		Addr:      req.GetAddr(),
		Subnet:    req.GetSubnet(),
		CPUMillis: int32(req.GetCpuMillis()),
		MemoryMiB: int32(req.GetMemoryMib()),
	})
	if err != nil {
		return nil, grpcError(err)
	}
	a.cp.live.touch(req.GetNode())
	return &andenv1.RegisterResponse{
		Epoch:               uint64(epoch),
		LeaseTtlMs:          uint32(leaseTTL / time.Millisecond),
		HeartbeatIntervalMs: uint32(heartbeatInterval / time.Millisecond),
		BlobsUrl:            a.cp.blobsURL(),
	}, nil
}

func (a *agentService) Heartbeat(ctx context.Context, req *andenv1.HeartbeatRequest) (*andenv1.HeartbeatResponse, error) {
	epoch, err := a.cp.store.NodeEpoch(ctx, req.GetNode())
	if err != nil {
		return nil, grpcError(err)
	}
	if err := checkEpoch(req.GetNode(), epoch, req.GetEpoch()); err != nil {
		return nil, err
	}
	a.cp.live.touch(req.GetNode())
	ready := a.cp.live.snapshot()
	services, deploys, err := a.cp.store.NodeAssignments(ctx, req.GetNode())
	if err != nil {
		return nil, grpcError(err)
	}
	assignments, err := toAssignments(services)
	if err != nil {
		return nil, grpcError(err)
	}
	if assignments, err = deployAssignments(req.GetNode(), assignments, deploys); err != nil {
		return nil, grpcError(err)
	}
	if slices.ContainsFunc(assignments, func(as *andenv1.Assignment) bool { return as.GetSpec().GetSingleton() }) {
		instances, err := a.cp.store.Instances(ctx)
		if err != nil {
			return nil, grpcError(err)
		}
		assignments = holdSingletons(req.GetNode(), assignments, instances, ready)
	}
	return &andenv1.HeartbeatResponse{LeaseTtlMs: uint32(leaseTTL / time.Millisecond), Assignments: assignments}, nil
}

func (a *agentService) ReportInstances(ctx context.Context, req *andenv1.ReportInstancesRequest) (*andenv1.ReportInstancesResponse, error) {
	instances := make([]store.Instance, 0, len(req.GetInstances()))
	for _, in := range req.GetInstances() {
		if in.GetId() == "" || in.GetService() == "" {
			return nil, status.Error(codes.InvalidArgument, "instances need an id and a service")
		}
		instances = append(instances, store.Instance{
			ID:         in.GetId(),
			Service:    in.GetService(),
			Version:    int64(in.GetVersion()),
			Generation: int64(in.GetGeneration()),
			State:      in.GetState().String(),
			Addr:       in.GetAddr(),
			Seccomp:    in.GetSeccomp(),
			Route:      in.GetRoute(),
		})
	}
	if err := a.cp.store.ReplaceInstances(ctx, req.GetNode(), int64(req.GetEpoch()), instances); err != nil {
		return nil, grpcError(err)
	}
	a.cp.live.touch(req.GetNode())
	a.cp.rec.Kick()
	return &andenv1.ReportInstancesResponse{}, nil
}

// errNothingToChange leaves a service as it is: an intent can arrive when the
// service has moved on, or when the desired state already says the same.
var errNothingToChange = errors.New("nothing to change")

// applyIntent applies what a node decided about a service placed on it.
func applyIntent(s *store.Service, node string, generation uint64, awake bool) error {
	if s.DesiredNode != node || uint64(s.Generation) != generation || s.DesiredAwake == awake {
		return errNothingToChange
	}
	if awake {
		wantAwake(s)
	} else {
		wantAsleep(s)
	}
	return nil
}

// SetAwake records in the desired state a sleep or a wake that the node already
// carried out: a wake waits for no one, and durable state must not contradict it.
func (a *agentService) SetAwake(ctx context.Context, req *andenv1.SetAwakeRequest) (*andenv1.SetAwakeResponse, error) {
	epoch, err := a.cp.store.NodeEpoch(ctx, req.GetNode())
	if err != nil {
		return nil, grpcError(err)
	}
	if err := checkEpoch(req.GetNode(), epoch, req.GetEpoch()); err != nil {
		return nil, err
	}
	a.cp.live.touch(req.GetNode())
	_, err = a.cp.store.UpdateDesired(ctx, req.GetService(), func(s *store.Service) error {
		return applyIntent(s, req.GetNode(), req.GetGeneration(), req.GetAwake())
	})
	if err != nil && !errors.Is(err, errNothingToChange) && !errors.Is(err, store.ErrNotFound) {
		return nil, grpcError(err)
	}
	a.cp.rec.Kick()
	return &andenv1.SetAwakeResponse{}, nil
}
