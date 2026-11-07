package controlplane

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	andenv1 "github.com/CesarManzoCode/anden/gen/anden/v1"
	"github.com/CesarManzoCode/anden/internal/store"
)

func validSpec() *andenv1.ServiceSpec {
	return &andenv1.ServiceSpec{
		Name:        "echo",
		ImageDigest: "sha256:" + strings.Repeat("ab", 32),
		Port:        8080,
		CpuMillis:   100,
		MemoryMib:   64,
	}
}

func TestNormalizeSpecDefaults(t *testing.T) {
	in := validSpec()
	s, err := normalizeSpec(in)
	if err != nil {
		t.Fatal(err)
	}
	if s.GetIdleSeconds() != 60 || s.GetHealthcheckTimeoutSeconds() != 60 || s.GetOverlapSeconds() != 20 || s.GetDrainingSeconds() != 20 {
		t.Errorf("defaults: %v", s)
	}
	if s.GetWakeRoute() != andenv1.WakeRoute_WAKE_ROUTE_RELAUNCH || !s.GetHealthcheck().GetTcp() {
		t.Errorf("route or healthcheck: %v", s)
	}
	if in.IdleSeconds != nil {
		t.Errorf("normalizeSpec modified its input")
	}
}

func TestNormalizeSpecKeepsExplicitZeroIdle(t *testing.T) {
	in := validSpec()
	in.IdleSeconds = proto.Uint32(0)
	s, err := normalizeSpec(in)
	if err != nil {
		t.Fatal(err)
	}
	if s.IdleSeconds == nil || *s.IdleSeconds != 0 {
		t.Fatalf("idle_seconds = 0 must mean never sleep, got %v", s.IdleSeconds)
	}
}

func TestNormalizeSpecRejects(t *testing.T) {
	cases := []struct {
		name string
		edit func(*andenv1.ServiceSpec)
		code codes.Code
	}{
		{"nil spec", nil, codes.InvalidArgument},
		{"uppercase name", func(s *andenv1.ServiceSpec) { s.Name = "Echo" }, codes.InvalidArgument},
		{"empty name", func(s *andenv1.ServiceSpec) { s.Name = "" }, codes.InvalidArgument},
		{"bad digest", func(s *andenv1.ServiceSpec) { s.ImageDigest = "sha256:abc" }, codes.InvalidArgument},
		{"no port", func(s *andenv1.ServiceSpec) { s.Port = 0 }, codes.InvalidArgument},
		{"port out of range", func(s *andenv1.ServiceSpec) { s.Port = 70000 }, codes.InvalidArgument},
		{"no cpu", func(s *andenv1.ServiceSpec) { s.CpuMillis = 0 }, codes.InvalidArgument},
		{"no memory", func(s *andenv1.ServiceSpec) { s.MemoryMib = 0 }, codes.InvalidArgument},
		{"env with =", func(s *andenv1.ServiceSpec) { s.Env = map[string]string{"A=B": "c"} }, codes.InvalidArgument},
		{"relative healthcheck path", func(s *andenv1.ServiceSpec) {
			s.Healthcheck = &andenv1.Healthcheck{Check: &andenv1.Healthcheck_Path{Path: "health"}}
		}, codes.InvalidArgument},
		{"relative volume", func(s *andenv1.ServiceSpec) {
			s.Volumes = []*andenv1.Volume{{Name: "data", MountPath: "data"}}
		}, codes.InvalidArgument},
		{"volume name out of its directory", func(s *andenv1.ServiceSpec) {
			s.Volumes = []*andenv1.Volume{{Name: "../data", MountPath: "/data"}}
		}, codes.InvalidArgument},
		{"volume name is a parent", func(s *andenv1.ServiceSpec) {
			s.Volumes = []*andenv1.Volume{{Name: "..", MountPath: "/data"}}
		}, codes.InvalidArgument},
		{"unknown route", func(s *andenv1.ServiceSpec) { s.WakeRoute = andenv1.WakeRoute(9) }, codes.InvalidArgument},
		{"restore with volumes", func(s *andenv1.ServiceSpec) {
			s.WakeRoute = andenv1.WakeRoute_WAKE_ROUTE_RESTORE
			s.Volumes = []*andenv1.Volume{{Name: "data", MountPath: "/data"}}
		}, codes.FailedPrecondition},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var spec *andenv1.ServiceSpec
			if c.edit != nil {
				spec = validSpec()
				c.edit(spec)
			}
			_, err := normalizeSpec(spec)
			if status.Code(err) != c.code {
				t.Fatalf("got %v, want %s", err, c.code)
			}
		})
	}
}

// Validation happens before the store is touched, so a service without a
// store is enough to see the codes the API returns.
func TestCreateAndDeployRestoreWithVolumesIsFailedPrecondition(t *testing.T) {
	spec := validSpec()
	spec.WakeRoute = andenv1.WakeRoute_WAKE_ROUTE_RESTORE
	spec.Volumes = []*andenv1.Volume{{Name: "data", MountPath: "/data"}}
	a := &andenService{cp: &ControlPlane{}}
	if _, err := a.CreateService(context.Background(), &andenv1.CreateServiceRequest{Spec: spec}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("create: %v", err)
	}
	if _, err := a.DeployService(context.Background(), &andenv1.DeployServiceRequest{Spec: spec}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("deploy: %v", err)
	}
	if _, err := a.CreateService(context.Background(), &andenv1.CreateServiceRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("create without spec: %v", err)
	}
}

func TestRestoreWithoutVolumesIsAccepted(t *testing.T) {
	spec := validSpec()
	spec.WakeRoute = andenv1.WakeRoute_WAKE_ROUTE_RESTORE
	if _, err := normalizeSpec(spec); err != nil {
		t.Fatal(err)
	}
}

func TestGRPCError(t *testing.T) {
	cases := map[error]codes.Code{
		store.ErrNotFound:                            codes.NotFound,
		store.ErrExists:                              codes.AlreadyExists,
		store.ErrStaleEpoch:                          codes.FailedPrecondition,
		store.ErrNoEdgePort:                          codes.ResourceExhausted,
		context.DeadlineExceeded:                     codes.DeadlineExceeded,
		status.Error(codes.Aborted, "kept as it is"): codes.Aborted,
	}
	for err, want := range cases {
		if got := status.Code(grpcError(err)); got != want {
			t.Errorf("%v: got %s, want %s", err, got, want)
		}
	}
}

func TestToProto(t *testing.T) {
	spec, err := normalizeSpec(validSpec())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := proto.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	svc := store.Service{Name: "echo", Spec: raw, Version: 3, State: stateActive, Node: "node2", Addr: "10.82.0.5", EdgePort: 30001}
	in := store.Instance{ID: "echo-4", Service: "echo", Node: "node2", Addr: "10.82.0.5", Version: 3, Generation: 4,
		State: instRunning, Seccomp: false, Route: "restore"}
	p, err := toProto(svc, []store.Instance{in})
	if err != nil {
		t.Fatal(err)
	}
	if p.GetState() != andenv1.ServiceState_SERVICE_STATE_ACTIVE || p.GetVersion() != 3 || p.GetEdgePort() != 30001 ||
		p.GetNode() != "node2" || p.GetSpec().GetName() != "echo" {
		t.Errorf("service: %v", p)
	}
	if p.GetInstance().GetId() != "echo-4" || p.GetInstance().GetSeccomp() || p.GetInstance().GetRoute() != "restore" {
		t.Errorf("instance: %v", p.GetInstance())
	}
	p, err = toProto(store.Service{Name: "echo", Spec: raw, State: stateAsleep}, nil)
	if err != nil || p.GetInstance() != nil || p.GetState() != andenv1.ServiceState_SERVICE_STATE_ASLEEP {
		t.Errorf("asleep: %v %v", p, err)
	}
}
