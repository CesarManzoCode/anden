package controlplane

import (
	"context"
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

// liveness tracks when each node was last heard from, with the monotonic
// clock. A node not heard from since andend started counts from the start,
// so a restart of the control plane does not declare every node dead.
type liveness struct {
	mu      sync.Mutex
	started time.Time
	seen    map[string]time.Time
}

func newLiveness() *liveness {
	return &liveness{started: time.Now(), seen: map[string]time.Time{}}
}

func (l *liveness) touch(node string) {
	l.mu.Lock()
	l.seen[node] = time.Now()
	l.mu.Unlock()
}

func (l *liveness) ready(node string) bool {
	l.mu.Lock()
	last, ok := l.seen[node]
	l.mu.Unlock()
	if !ok {
		last = l.started
	}
	return time.Since(last) < deadAfter
}

type agentService struct {
	andenv1.UnimplementedAgentControlServer
	cp *ControlPlane
}

// Register gives the node a new epoch; reports with an older one are refused.
func (a *agentService) Register(ctx context.Context, req *andenv1.RegisterRequest) (*andenv1.RegisterResponse, error) {
	if req.GetNode() == "" {
		return nil, status.Error(codes.InvalidArgument, "node is required")
	}
	epoch, err := a.cp.store.RegisterNode(ctx, store.Node{
		Name:      req.GetNode(),
		Addr:      req.GetAddr(),
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
	}, nil
}

func (a *agentService) Heartbeat(ctx context.Context, req *andenv1.HeartbeatRequest) (*andenv1.HeartbeatResponse, error) {
	epoch, err := a.cp.store.NodeEpoch(ctx, req.GetNode())
	if err != nil {
		return nil, grpcError(err)
	}
	if uint64(epoch) != req.GetEpoch() {
		return nil, status.Errorf(codes.FailedPrecondition, "epoch %d of %s is stale, current is %d", req.GetEpoch(), req.GetNode(), epoch)
	}
	a.cp.live.touch(req.GetNode())
	return &andenv1.HeartbeatResponse{LeaseTtlMs: uint32(leaseTTL / time.Millisecond)}, nil
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
