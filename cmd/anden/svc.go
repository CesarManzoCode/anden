package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	andenv1 "github.com/CesarManzoCode/anden/gen/anden/v1"
)

// listFlag collects a flag given several times.
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

// specFlags are the ServiceSpec fields of create and deploy.
type specFlags struct {
	image, wakeRoute, antiAffinity, healthPath                   string
	port, cpu, mem, pids, idle, healthTimeout, overlap, draining uint
	singleton                                                    bool
	env, args, volumes                                           listFlag
}

func (f *specFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.image, "image", "", "image digest, sha256:<hex>")
	fs.UintVar(&f.port, "port", 0, "TCP port the service listens on")
	fs.UintVar(&f.cpu, "cpu-millis", 0, "CPU in thousandths of a core")
	fs.UintVar(&f.mem, "memory-mib", 0, "memory limit in MiB")
	fs.UintVar(&f.pids, "pids-max", 0, "process limit; 0 is none")
	fs.Var(&f.env, "env", "KEY=VALUE, repeatable")
	fs.Var(&f.args, "arg", "process argument, repeatable and in order")
	fs.UintVar(&f.idle, "idle-seconds", 0, "idle window before sleeping; 0 never sleeps (default 60)")
	fs.StringVar(&f.wakeRoute, "wake-route", "relaunch", "relaunch or restore")
	fs.BoolVar(&f.singleton, "singleton", false, "never two active instances")
	fs.StringVar(&f.antiAffinity, "anti-affinity-group", "", "services of one group go to distinct nodes")
	fs.StringVar(&f.healthPath, "health-path", "", "HTTP healthcheck path; TCP when empty")
	fs.UintVar(&f.healthTimeout, "healthcheck-timeout-seconds", 0, "default 60")
	fs.UintVar(&f.overlap, "overlap-seconds", 0, "default 20")
	fs.UintVar(&f.draining, "draining-seconds", 0, "default 20")
	fs.Var(&f.volumes, "volume", "NAME:/mount/path, repeatable")
}

func (f *specFlags) spec(fs *flag.FlagSet, name string) (*andenv1.ServiceSpec, error) {
	s := &andenv1.ServiceSpec{
		Name:              name,
		ImageDigest:       f.image,
		Port:              uint32(f.port),
		CpuMillis:         uint32(f.cpu),
		MemoryMib:         uint32(f.mem),
		PidsMax:           uint32(f.pids),
		Args:              f.args,
		Singleton:         f.singleton,
		AntiAffinityGroup: f.antiAffinity,
	}
	switch f.wakeRoute {
	case "relaunch":
		s.WakeRoute = andenv1.WakeRoute_WAKE_ROUTE_RELAUNCH
	case "restore":
		s.WakeRoute = andenv1.WakeRoute_WAKE_ROUTE_RESTORE
	default:
		return nil, fmt.Errorf("--wake-route must be relaunch or restore")
	}
	if len(f.env) > 0 {
		s.Env = map[string]string{}
		for _, kv := range f.env {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return nil, fmt.Errorf("--env %q is not KEY=VALUE", kv)
			}
			s.Env[k] = v
		}
	}
	for _, vol := range f.volumes {
		n, p, ok := strings.Cut(vol, ":")
		if !ok {
			return nil, fmt.Errorf("--volume %q is not NAME:/path", vol)
		}
		s.Volumes = append(s.Volumes, &andenv1.Volume{Name: n, MountPath: p})
	}
	if f.healthPath != "" {
		s.Healthcheck = &andenv1.Healthcheck{Check: &andenv1.Healthcheck_Path{Path: f.healthPath}}
	}
	// Only flags given on the command line override the server defaults.
	fs.Visit(func(fl *flag.Flag) {
		switch fl.Name {
		case "idle-seconds":
			s.IdleSeconds = proto.Uint32(uint32(f.idle))
		case "healthcheck-timeout-seconds":
			s.HealthcheckTimeoutSeconds = proto.Uint32(uint32(f.healthTimeout))
		case "overlap-seconds":
			s.OverlapSeconds = proto.Uint32(uint32(f.overlap))
		case "draining-seconds":
			s.DrainingSeconds = proto.Uint32(uint32(f.draining))
		}
	})
	return s, nil
}

// apiSocket reads api_socket from STATE/dc.json.
func apiSocket(state string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(state, "dc.json"))
	if err != nil {
		return "", err
	}
	var dc struct {
		APISocket string `json:"api_socket"`
	}
	if err := json.Unmarshal(raw, &dc); err != nil {
		return "", fmt.Errorf("dc.json: %w", err)
	}
	if dc.APISocket == "" {
		return "", fmt.Errorf("dc.json has no api_socket")
	}
	return dc.APISocket, nil
}

func runSvc(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd := args[0]
	fs := flag.NewFlagSet("anden svc "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	state := fs.String("state", defaultState, "state directory with dc.json")
	socket := fs.String("socket", "", "API Unix socket; overrides api_socket of dc.json")
	jsonOut := fs.Bool("json", false, "print a JSON object on stdout")
	timeout := fs.Duration("timeout", 10*time.Second, "timeout of the call")
	var name, node *string
	var sf *specFlags
	switch cmd {
	case "create", "deploy":
		name = fs.String("name", "", "service name")
		sf = &specFlags{}
		sf.register(fs)
	case "get", "sleep", "wake", "delete":
		name = fs.String("name", "", "service name")
	case "move":
		name = fs.String("name", "", "service name")
		node = fs.String("node", "", "destination node")
	case "list":
	default:
		fmt.Fprint(stderr, usage)
		return 2
	}
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 || (name != nil && *name == "") || (node != nil && *node == "") {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var spec *andenv1.ServiceSpec
	if sf != nil {
		var err error
		if spec, err = sf.spec(fs, *name); err != nil {
			fmt.Fprintln(stderr, "anden:", err)
			return 2
		}
	}

	path := *socket
	if path == "" {
		var err error
		if path, err = apiSocket(*state); err != nil {
			fmt.Fprintln(stderr, "anden:", err)
			return 1
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		fmt.Fprintln(stderr, "anden:", err)
		return 1
	}
	conn, err := grpc.NewClient("unix://"+abs, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Fprintln(stderr, "anden:", err)
		return 1
	}
	defer conn.Close()
	client := andenv1.NewAndenClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	var resp proto.Message
	switch cmd {
	case "create":
		resp, err = client.CreateService(ctx, &andenv1.CreateServiceRequest{Spec: spec})
	case "deploy":
		resp, err = client.DeployService(ctx, &andenv1.DeployServiceRequest{Spec: spec})
	case "get":
		resp, err = client.GetService(ctx, &andenv1.GetServiceRequest{Name: *name})
	case "sleep":
		resp, err = client.SleepService(ctx, &andenv1.SleepServiceRequest{Name: *name})
	case "wake":
		resp, err = client.WakeService(ctx, &andenv1.WakeServiceRequest{Name: *name})
	case "delete":
		resp, err = client.DeleteService(ctx, &andenv1.DeleteServiceRequest{Name: *name})
	case "move":
		resp, err = client.MoveService(ctx, &andenv1.MoveServiceRequest{Name: *name, Node: *node})
	case "list":
		resp, err = client.ListServices(ctx, &andenv1.ListServicesRequest{})
	}
	if err != nil {
		fmt.Fprintln(stderr, "anden:", err)
		return 1
	}
	if *jsonOut {
		out, err := protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}.Marshal(resp)
		if err != nil {
			fmt.Fprintln(stderr, "anden:", err)
			return 1
		}
		fmt.Fprintln(stdout, string(out))
		return 0
	}
	services := []*andenv1.Service{}
	switch r := resp.(type) {
	case *andenv1.Service:
		services = append(services, r)
	case *andenv1.ListServicesResponse:
		services = r.GetServices()
	}
	for _, s := range services {
		fmt.Fprintf(stdout, "%s\t%s\tv%d\t%s\t%s\n", s.GetSpec().GetName(), s.GetState(), s.GetVersion(), s.GetNode(), s.GetAddr())
	}
	return 0
}
