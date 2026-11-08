// Command apiclient walks the public API from outside the product, using only
// the generated code: create, get, sleep, wake, delete and get_after_delete.
// create, sleep and wake change the desired state, so it waits for the service
// to get there and reports the state it read last. It prints one JSON line per
// operation: {"op", "grpc_code", "state"}, with the numeric gRPC code.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	andenv1 "github.com/CesarManzoCode/anden/gen/anden/v1"
)

const settlePoll = 100 * time.Millisecond

type line struct {
	Op       string `json:"op"`
	GRPCCode uint32 `json:"grpc_code"`
	State    string `json:"state"`
}

// settle reads the service until it reaches want or ctx ends, and returns what
// it read last.
func settle(ctx context.Context, client andenv1.AndenClient, name string, want andenv1.ServiceState) (*andenv1.Service, error) {
	for {
		svc, err := client.GetService(ctx, &andenv1.GetServiceRequest{Name: name})
		if err != nil || svc.GetState() == want {
			return svc, err
		}
		select {
		case <-ctx.Done():
			return svc, nil
		case <-time.After(settlePoll):
		}
	}
}

func main() {
	socket := flag.String("socket", "", "API Unix socket (api_socket of dc.json)")
	name := flag.String("name", "apiclient", "service name")
	digest := flag.String("image-digest", "", "image digest, sha256:<hex>")
	port := flag.Uint("port", 8080, "service port")
	timeout := flag.Duration("timeout", 10*time.Second, "timeout of each call")
	settleTimeout := flag.Duration("settle-timeout", 60*time.Second, "how long a service gets to reach the state asked for")
	flag.Parse()
	if *socket == "" || *digest == "" {
		flag.Usage()
		os.Exit(2)
	}
	abs, err := filepath.Abs(*socket)
	if err != nil {
		fmt.Fprintln(os.Stderr, "apiclient:", err)
		os.Exit(2)
	}
	conn, err := grpc.NewClient("unix://"+abs, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Fprintln(os.Stderr, "apiclient:", err)
		os.Exit(1)
	}
	defer conn.Close()
	client := andenv1.NewAndenClient(conn)

	ops := []struct {
		op     string
		want   codes.Code
		settle andenv1.ServiceState
		call   func(context.Context) (*andenv1.Service, error)
	}{
		{"create", codes.OK, andenv1.ServiceState_SERVICE_STATE_ACTIVE, func(ctx context.Context) (*andenv1.Service, error) {
			return client.CreateService(ctx, &andenv1.CreateServiceRequest{Spec: &andenv1.ServiceSpec{
				Name: *name, ImageDigest: *digest, Port: uint32(*port), CpuMillis: 100, MemoryMib: 64,
			}})
		}},
		{"get", codes.OK, andenv1.ServiceState_SERVICE_STATE_UNSPECIFIED, func(ctx context.Context) (*andenv1.Service, error) {
			return client.GetService(ctx, &andenv1.GetServiceRequest{Name: *name})
		}},
		{"sleep", codes.OK, andenv1.ServiceState_SERVICE_STATE_ASLEEP, func(ctx context.Context) (*andenv1.Service, error) {
			return client.SleepService(ctx, &andenv1.SleepServiceRequest{Name: *name})
		}},
		{"wake", codes.OK, andenv1.ServiceState_SERVICE_STATE_ACTIVE, func(ctx context.Context) (*andenv1.Service, error) {
			return client.WakeService(ctx, &andenv1.WakeServiceRequest{Name: *name})
		}},
		{"delete", codes.OK, andenv1.ServiceState_SERVICE_STATE_UNSPECIFIED, func(ctx context.Context) (*andenv1.Service, error) {
			return client.DeleteService(ctx, &andenv1.DeleteServiceRequest{Name: *name})
		}},
		{"get_after_delete", codes.NotFound, andenv1.ServiceState_SERVICE_STATE_UNSPECIFIED, func(ctx context.Context) (*andenv1.Service, error) {
			return client.GetService(ctx, &andenv1.GetServiceRequest{Name: *name})
		}},
	}
	rc := 0
	enc := json.NewEncoder(os.Stdout)
	for _, o := range ops {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		svc, err := o.call(ctx)
		cancel()
		if err == nil && o.settle != andenv1.ServiceState_SERVICE_STATE_UNSPECIFIED {
			ctx, cancel := context.WithTimeout(context.Background(), *settleTimeout)
			svc, err = settle(ctx, client, *name, o.settle)
			cancel()
			if err == nil && svc.GetState() != o.settle {
				fmt.Fprintf(os.Stderr, "apiclient: %s: want %s, got %s\n", o.op, o.settle, svc.GetState())
				rc = 1
			}
		}
		l := line{Op: o.op, GRPCCode: uint32(status.Code(err))}
		if err == nil {
			l.State = svc.GetState().String()
		}
		if status.Code(err) != o.want {
			fmt.Fprintf(os.Stderr, "apiclient: %s: want %s, got %v\n", o.op, o.want, err)
			rc = 1
		}
		if err := enc.Encode(l); err != nil {
			fmt.Fprintln(os.Stderr, "apiclient:", err)
			os.Exit(1)
		}
	}
	os.Exit(rc)
}
