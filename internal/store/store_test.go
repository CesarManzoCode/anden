package store

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestConnStringUsesUnixSocket(t *testing.T) {
	cfg, err := pgconn.ParseConfig(ConnString("/tmp/anden-x/pg dir", "an'den", "postgres"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "/tmp/anden-x/pg dir" || cfg.User != "an'den" || cfg.Database != "postgres" {
		t.Fatalf("got host=%q user=%q db=%q", cfg.Host, cfg.User, cfg.Database)
	}
	if network, _ := pgconn.NetworkAddress(cfg.Host, cfg.Port); network != "unix" {
		t.Fatalf("network %q, want unix", network)
	}
}

func TestSchemaIsEmbedded(t *testing.T) {
	for _, table := range []string{"services", "nodes", "instances"} {
		if !strings.Contains(schemaV1, "CREATE TABLE "+table+" (") {
			t.Errorf("schema has no table %s", table)
		}
	}
}

func TestMigrationsAreInOrder(t *testing.T) {
	if len(migrations) != 2 || migrations[0] != schemaV1 || !strings.Contains(migrations[1], "CREATE TABLE deployments (") {
		t.Fatalf("migrations are not v1 then v2 with deployments")
	}
}
