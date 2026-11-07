package controlplane

import (
	"context"
	"errors"
	"path"
	"regexp"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	andenv1 "github.com/CesarManzoCode/anden/gen/anden/v1"
	"github.com/CesarManzoCode/anden/internal/store"
)

// Defaults of D05 and D12 for fields the request leaves unset.
const (
	defaultIdleSeconds        = 60
	defaultHealthcheckTimeout = 60
	defaultOverlapSeconds     = 20
	defaultDrainingSeconds    = 20
)

var (
	// A service name is a DNS label: it becomes <name>.anden.internal (D22).
	nameRE   = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)
	digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

type andenService struct {
	andenv1.UnimplementedAndenServer
	cp *ControlPlane
}

// normalizeSpec validates a spec and returns a copy with defaults filled in.
func normalizeSpec(spec *andenv1.ServiceSpec) (*andenv1.ServiceSpec, error) {
	if spec == nil {
		return nil, status.Error(codes.InvalidArgument, "spec is required")
	}
	s := proto.CloneOf(spec)
	invalid := func(msg string) error { return status.Error(codes.InvalidArgument, msg) }
	switch {
	case !nameRE.MatchString(s.Name):
		return nil, invalid("name must be a DNS label of lowercase letters, digits and hyphens")
	case !digestRE.MatchString(s.ImageDigest):
		return nil, invalid("image_digest must be sha256:<64 hex digits>")
	case s.Port == 0 || s.Port > 65535:
		return nil, invalid("port must be between 1 and 65535")
	case s.CpuMillis == 0:
		return nil, invalid("cpu_millis must be positive")
	case s.MemoryMib == 0:
		return nil, invalid("memory_mib must be positive")
	}
	for k := range s.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") {
			return nil, invalid("env names must be non-empty and contain no '=' or NUL")
		}
	}
	switch s.WakeRoute {
	case andenv1.WakeRoute_WAKE_ROUTE_UNSPECIFIED:
		s.WakeRoute = andenv1.WakeRoute_WAKE_ROUTE_RELAUNCH
	case andenv1.WakeRoute_WAKE_ROUTE_RELAUNCH, andenv1.WakeRoute_WAKE_ROUTE_RESTORE:
	default:
		return nil, invalid("unknown wake_route")
	}
	switch hc := s.Healthcheck.GetCheck().(type) {
	case nil:
		s.Healthcheck = &andenv1.Healthcheck{Check: &andenv1.Healthcheck_Tcp{Tcp: true}}
	case *andenv1.Healthcheck_Path:
		if !strings.HasPrefix(hc.Path, "/") {
			return nil, invalid("healthcheck path must start with /")
		}
	case *andenv1.Healthcheck_Tcp:
		if !hc.Tcp {
			return nil, invalid("healthcheck needs a path or tcp = true")
		}
	}
	for _, v := range s.Volumes {
		// The name becomes a directory of the node.
		name := v.GetName()
		if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
			return nil, invalid("a volume name cannot be empty, a relative path or contain /")
		}
		if !path.IsAbs(v.GetMountPath()) || path.Clean(v.GetMountPath()) != v.GetMountPath() {
			return nil, invalid("volumes need a clean absolute mount_path")
		}
	}
	// D03: the restore route has no volumes.
	if s.WakeRoute == andenv1.WakeRoute_WAKE_ROUTE_RESTORE && len(s.Volumes) > 0 {
		return nil, status.Error(codes.FailedPrecondition, "wake_route restore does not admit volumes")
	}
	if s.IdleSeconds == nil {
		s.IdleSeconds = proto.Uint32(defaultIdleSeconds)
	}
	if s.HealthcheckTimeoutSeconds == nil {
		s.HealthcheckTimeoutSeconds = proto.Uint32(defaultHealthcheckTimeout)
	}
	if s.OverlapSeconds == nil {
		s.OverlapSeconds = proto.Uint32(defaultOverlapSeconds)
	}
	if s.DrainingSeconds == nil {
		s.DrainingSeconds = proto.Uint32(defaultDrainingSeconds)
	}
	return s, nil
}

// grpcError maps store and context errors to gRPC status errors.
func grpcError(err error) error {
	if _, ok := status.FromError(err); ok {
		return err
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return status.FromContextError(err).Err()
	case errors.Is(err, store.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, store.ErrExists):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, store.ErrStaleEpoch), errors.Is(err, store.ErrDeployInProgress):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, store.ErrNoEdgePort):
		return status.Error(codes.ResourceExhausted, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}

func toProto(svc store.Service, instances []store.Instance) (*andenv1.Service, error) {
	spec := &andenv1.ServiceSpec{}
	if err := proto.Unmarshal(svc.Spec, spec); err != nil {
		return nil, status.Errorf(codes.Internal, "stored spec of %s: %v", svc.Name, err)
	}
	out := &andenv1.Service{
		Spec:     spec,
		State:    andenv1.ServiceState(andenv1.ServiceState_value[svc.State]),
		Version:  uint64(svc.Version),
		Node:     svc.Node,
		Addr:     svc.Addr,
		EdgePort: uint32(svc.EdgePort),
	}
	if in := liveInstance(instances); in != nil {
		out.Instance = &andenv1.Instance{
			Id:         in.ID,
			Node:       in.Node,
			Addr:       in.Addr,
			Version:    uint64(in.Version),
			Generation: uint64(in.Generation),
			State:      andenv1.InstanceState(andenv1.InstanceState_value[in.State]),
			Seccomp:    in.Seccomp,
			Route:      in.Route,
		}
	}
	return out, nil
}

// reply converts svc with the instances reported for it.
func (a *andenService) reply(ctx context.Context, svc store.Service, err error) (*andenv1.Service, error) {
	if err != nil {
		return nil, grpcError(err)
	}
	a.cp.rec.Kick()
	instances, err := a.cp.store.Instances(ctx)
	if err != nil {
		return nil, grpcError(err)
	}
	var own []store.Instance
	for _, in := range liveOnly(instances, a.cp.live.ready) {
		if in.Service == svc.Name {
			own = append(own, in)
		}
	}
	return toProto(svc, own)
}

func (a *andenService) CreateService(ctx context.Context, req *andenv1.CreateServiceRequest) (*andenv1.Service, error) {
	spec, err := normalizeSpec(req.GetSpec())
	if err != nil {
		return nil, err
	}
	raw, err := proto.Marshal(spec)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	svc, err := a.cp.store.CreateService(ctx, spec.Name, raw, stateCreating)
	return a.reply(ctx, svc, err)
}

func (a *andenService) GetService(ctx context.Context, req *andenv1.GetServiceRequest) (*andenv1.Service, error) {
	svc, err := a.cp.store.GetService(ctx, req.GetName())
	return a.reply(ctx, svc, err)
}

func (a *andenService) ListServices(ctx context.Context, _ *andenv1.ListServicesRequest) (*andenv1.ListServicesResponse, error) {
	services, err := a.cp.store.ListServices(ctx, false)
	if err != nil {
		return nil, grpcError(err)
	}
	instances, err := a.cp.store.Instances(ctx)
	if err != nil {
		return nil, grpcError(err)
	}
	byService := map[string][]store.Instance{}
	for _, in := range liveOnly(instances, a.cp.live.ready) {
		byService[in.Service] = append(byService[in.Service], in)
	}
	out := &andenv1.ListServicesResponse{}
	for _, svc := range services {
		p, err := toProto(svc, byService[svc.Name])
		if err != nil {
			return nil, err
		}
		out.Services = append(out.Services, p)
	}
	return out, nil
}

// wantAsleep and wantAwake are the desired change of a sleep and of a wake,
// whether the API asked for it or a node decided it.
func wantAsleep(s *store.Service) {
	s.DesiredAwake = false
	if s.State != stateAsleep {
		s.State = stateSleeping
	}
}

func wantAwake(s *store.Service) {
	s.DesiredAwake = true
	// The instance of a failed generation stays for inspection; only a
	// new generation replaces it.
	if s.State == stateFailed {
		s.Generation++
	}
	switch s.State {
	case stateSleeping, stateAsleep, stateFailed:
		s.State = stateWaking
	}
}

func (a *andenService) SleepService(ctx context.Context, req *andenv1.SleepServiceRequest) (*andenv1.Service, error) {
	svc, err := a.cp.store.UpdateDesired(ctx, req.GetName(), func(s *store.Service) error {
		wantAsleep(s)
		return nil
	})
	return a.reply(ctx, svc, err)
}

func (a *andenService) WakeService(ctx context.Context, req *andenv1.WakeServiceRequest) (*andenv1.Service, error) {
	svc, err := a.cp.store.UpdateDesired(ctx, req.GetName(), func(s *store.Service) error {
		wantAwake(s)
		return nil
	})
	return a.reply(ctx, svc, err)
}

// DeleteService marks the service deleted; the reconciler purges it once no
// instance is left. It is not found from this call on.
func (a *andenService) DeleteService(ctx context.Context, req *andenv1.DeleteServiceRequest) (*andenv1.Service, error) {
	svc, err := a.cp.store.UpdateDesired(ctx, req.GetName(), func(s *store.Service) error {
		s.Deleted = true
		return nil
	})
	return a.reply(ctx, svc, err)
}

// DeployService rolls out a new spec as the next version, in a new generation
// (D24). On an active service the version only becomes the desired one once
// the new instance passes its healthcheck, and the reconciler carries the
// rollout from there; on any other, a sleeping one above all, it only changes
// the desired version, which applies at the next wake.
func (a *andenService) DeployService(ctx context.Context, req *andenv1.DeployServiceRequest) (*andenv1.Service, error) {
	spec, err := normalizeSpec(req.GetSpec())
	if err != nil {
		return nil, err
	}
	raw, err := proto.Marshal(spec)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	svc, _, err := a.cp.store.Deploy(ctx, spec.Name, raw, phaseStarting, time.Now(), rolloutWanted)
	return a.reply(ctx, svc, err)
}

// MoveService makes another node the home of the service, in a new generation
// and with an address of that node's subnet, which the next reconciler pass
// assigns; the edge and the DNS follow the address. The service moves asleep:
// an awake one sleeps first, and its next wake happens in the new home (D22).
func (a *andenService) MoveService(ctx context.Context, req *andenv1.MoveServiceRequest) (*andenv1.Service, error) {
	nodes, err := a.cp.store.ListNodes(ctx)
	if err != nil {
		return nil, grpcError(err)
	}
	known := false
	for _, n := range nodes {
		known = known || n.Name == req.GetNode()
	}
	if !known {
		return nil, status.Errorf(codes.NotFound, "node %q is not registered", req.GetNode())
	}
	svc, err := a.cp.store.UpdateDesired(ctx, req.GetName(), func(s *store.Service) error {
		moveTo(s, req.GetNode())
		return nil
	})
	return a.reply(ctx, svc, err)
}

// moveTo is the desired change of a move: asleep, with node as home and no
// address until the reconciler gives one of that node. Moving to the same home
// changes nothing.
func moveTo(s *store.Service, node string) {
	if s.DesiredNode == node {
		return
	}
	wantAsleep(s)
	s.DesiredNode, s.DesiredAddr = node, ""
	s.Generation++
}

func (a *andenService) ListNodes(ctx context.Context, _ *andenv1.ListNodesRequest) (*andenv1.ListNodesResponse, error) {
	nodes, err := a.cp.store.ListNodes(ctx)
	if err != nil {
		return nil, grpcError(err)
	}
	out := &andenv1.ListNodesResponse{}
	for _, n := range nodes {
		state := andenv1.NodeState_NODE_STATE_DEAD
		if a.cp.live.ready(n.Name) {
			state = andenv1.NodeState_NODE_STATE_READY
		}
		out.Nodes = append(out.Nodes, &andenv1.Node{
			Name:      n.Name,
			State:     state,
			Epoch:     uint64(n.Epoch),
			Addr:      n.Addr,
			CpuMillis: uint32(n.CPUMillis),
			MemoryMib: uint32(n.MemoryMiB),
		})
	}
	return out, nil
}
