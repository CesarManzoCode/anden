// Command apiclient walks the public API from outside the product, using only
// the generated code: create, get, sleep, wake, delete and get_after_delete.
// It prints one JSON line per operation: {"op", "grpc_code", "state"}.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	andenv1 "github.com/CesarManzoCode/anden/gen/anden/v1"
)

type line struct {
	Op       string `json:"op"`
	GRPCCode string `json:"grpc_code"`
	State    string `json:"state"`
}

// codeName turns a code like NotFound into its canonical name NOT_FOUND.
func codeName(err error) string {
	var b strings.Builder
	prev := rune(0)
	for _, r := range status.Code(err).String() {
		if unicode.IsUpper(r) && unicode.IsLower(prev) {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToUpper(r))
		prev = r
	}
	return b.String()
}

func main() {
	socket := flag.String("socket", "", "API Unix socket (api_socket of dc.json)")
	name := flag.String("name", "apiclient", "service name")
	digest := flag.String("image-digest", "", "image digest, sha256:<hex>")
	port := flag.Uint("port", 8080, "service port")
	timeout := flag.Duration("timeout", 10*time.Second, "timeout of each call")
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
		op   string
		want string
		call func(context.Context) (*andenv1.Service, error)
	}{
		{"create", "OK", func(ctx context.Context) (*andenv1.Service, error) {
			return client.CreateService(ctx, &andenv1.CreateServiceRequest{Spec: &andenv1.ServiceSpec{
				Name: *name, ImageDigest: *digest, Port: uint32(*port), CpuMillis: 100, MemoryMib: 64,
			}})
		}},
		{"get", "OK", func(ctx context.Context) (*andenv1.Service, error) {
			return client.GetService(ctx, &andenv1.GetServiceRequest{Name: *name})
		}},
		{"sleep", "OK", func(ctx context.Context) (*andenv1.Service, error) {
			return client.SleepService(ctx, &andenv1.SleepServiceRequest{Name: *name})
		}},
		{"wake", "OK", func(ctx context.Context) (*andenv1.Service, error) {
			return client.WakeService(ctx, &andenv1.WakeServiceRequest{Name: *name})
		}},
		{"delete", "OK", func(ctx context.Context) (*andenv1.Service, error) {
			return client.DeleteService(ctx, &andenv1.DeleteServiceRequest{Name: *name})
		}},
		{"get_after_delete", "NOT_FOUND", func(ctx context.Context) (*andenv1.Service, error) {
			return client.GetService(ctx, &andenv1.GetServiceRequest{Name: *name})
		}},
	}
	rc := 0
	enc := json.NewEncoder(os.Stdout)
	for _, o := range ops {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		svc, err := o.call(ctx)
		cancel()
		l := line{Op: o.op, GRPCCode: codeName(err)}
		if err == nil {
			l.State = svc.GetState().String()
		}
		if l.GRPCCode != o.want {
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
