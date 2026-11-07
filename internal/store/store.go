// Package store keeps Andén's desired and observed state in PostgreSQL,
// reached with pgx v5 over a Unix socket (contract D02). The schema is
// embedded and migrated when the store opens.
package store

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaV1 string

// migrationLock is the advisory lock key held while migrating, so two
// control planes starting together do not apply the schema twice.
const migrationLock = 0x616e64656e

// Edge ports are assigned from 30000 (D21).
const (
	edgePortFirst = 30000
	edgePortLast  = 32767
)

var (
	ErrNotFound   = errors.New("not found")
	ErrExists     = errors.New("already exists")
	ErrStaleEpoch = errors.New("stale node epoch")
	ErrNoEdgePort = errors.New("no free edge port")
)

// Service is one row of desired plus observed state. Spec is opaque to the
// store: the control plane encodes it.
type Service struct {
	Name         string
	Spec         []byte
	Version      int64
	DesiredAwake bool
	DesiredNode  string
	Deleted      bool
	// Revision grows with every desired change; observed writes carry the
	// revision they were computed from and lose against a newer one.
	Revision int64
	State    string
	Node     string
	Addr     string
	EdgePort int32
}

// Observed is what the reconciler writes back for a service.
type Observed struct {
	State string
	Node  string
	Addr  string
}

type Node struct {
	Name      string
	Addr      string
	Epoch     int64
	CPUMillis int32
	MemoryMiB int32
}

type Instance struct {
	ID         string
	Service    string
	Node       string
	Version    int64
	Generation int64
	State      string
	Addr       string
	Seccomp    bool
	Route      string
}

type Store struct {
	pool *pgxpool.Pool
}

// ConnString builds a pgx connection string for a server listening only on
// the Unix socket in socketDir.
func ConnString(socketDir, user, database string) string {
	return fmt.Sprintf("host=%s user=%s dbname=%s", quote(socketDir), quote(user), quote(database))
}

func quote(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	return "'" + strings.ReplaceAll(v, "'", `\'`) + "'"
}

// Open connects and applies the embedded schema if it is missing.
func Open(ctx context.Context, connString string) (*Store, error) {
	pool, err := pgxpool.New(ctx, connString)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if err := migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

func migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(migrationLock)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations (version integer PRIMARY KEY)"); err != nil {
			return err
		}
		var version int
		if err := tx.QueryRow(ctx, "SELECT coalesce(max(version), 0) FROM schema_migrations").Scan(&version); err != nil {
			return err
		}
		if version >= 1 {
			return nil
		}
		// Without arguments pgx uses the simple protocol, which accepts
		// several statements in one call.
		if _, err := tx.Exec(ctx, schemaV1); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES (1)")
		return err
	})
}

const serviceColumns = "name, spec, version, desired_awake, desired_node, deleted, revision, state, node, addr, edge_port"

func scanService(row pgx.Row) (Service, error) {
	var v Service
	err := row.Scan(&v.Name, &v.Spec, &v.Version, &v.DesiredAwake, &v.DesiredNode, &v.Deleted,
		&v.Revision, &v.State, &v.Node, &v.Addr, &v.EdgePort)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrNotFound
	}
	return v, err
}

// CreateService inserts version 1 of a service, awake, with the lowest free
// edge port.
func (s *Store) CreateService(ctx context.Context, name string, spec []byte, state string) (Service, error) {
	var out Service
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Serializes port assignment between concurrent creates.
		if _, err := tx.Exec(ctx, "LOCK TABLE services IN SHARE ROW EXCLUSIVE MODE"); err != nil {
			return err
		}
		var port *int32
		err := tx.QueryRow(ctx, `SELECT min(p)::integer FROM generate_series($1::integer, $2::integer) AS p
			WHERE NOT EXISTS (SELECT 1 FROM services WHERE edge_port = p)`, edgePortFirst, edgePortLast).Scan(&port)
		if err != nil {
			return err
		}
		if port == nil {
			return ErrNoEdgePort
		}
		out, err = scanService(tx.QueryRow(ctx, `INSERT INTO services (name, spec, version, desired_awake, state, edge_port)
			VALUES ($1, $2, 1, true, $3, $4) RETURNING `+serviceColumns, name, spec, state, *port))
		return err
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Service{}, ErrExists
	}
	return out, err
}

// GetService returns a service that is not marked as deleted.
func (s *Store) GetService(ctx context.Context, name string) (Service, error) {
	return scanService(s.pool.QueryRow(ctx, "SELECT "+serviceColumns+" FROM services WHERE name = $1 AND NOT deleted", name))
}

// ListServices returns services ordered by name; deleted ones only when
// includeDeleted is set.
func (s *Store) ListServices(ctx context.Context, includeDeleted bool) ([]Service, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+serviceColumns+" FROM services WHERE $1 OR NOT deleted ORDER BY name", includeDeleted)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Service, error) { return scanService(row) })
}

// UpdateDesired locks a live service, lets fn change its desired fields and
// state, and stores the result with the next revision. An error from fn
// aborts the update and is returned as is.
func (s *Store) UpdateDesired(ctx context.Context, name string, fn func(*Service) error) (Service, error) {
	var out Service
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		cur, err := scanService(tx.QueryRow(ctx, "SELECT "+serviceColumns+" FROM services WHERE name = $1 AND NOT deleted FOR UPDATE", name))
		if err != nil {
			return err
		}
		if err := fn(&cur); err != nil {
			return err
		}
		out, err = scanService(tx.QueryRow(ctx, `UPDATE services SET spec = $2, version = $3, desired_awake = $4,
			desired_node = $5, deleted = $6, state = $7, revision = revision + 1 WHERE name = $1 RETURNING `+serviceColumns,
			name, cur.Spec, cur.Version, cur.DesiredAwake, cur.DesiredNode, cur.Deleted, cur.State))
		return err
	})
	return out, err
}

// SetObserved writes observed state if the service is still at revision. It
// reports whether the row was written.
func (s *Store) SetObserved(ctx context.Context, name string, revision int64, obs Observed) (bool, error) {
	tag, err := s.pool.Exec(ctx, "UPDATE services SET state = $3, node = $4, addr = $5 WHERE name = $1 AND revision = $2",
		name, revision, obs.State, obs.Node, obs.Addr)
	return tag.RowsAffected() == 1, err
}

// Purge removes a service marked as deleted, if still at revision.
func (s *Store) Purge(ctx context.Context, name string, revision int64) error {
	_, err := s.pool.Exec(ctx, "DELETE FROM services WHERE name = $1 AND revision = $2 AND deleted", name, revision)
	return err
}

// RegisterNode records a node and returns its new epoch, one more than the
// last one it had.
func (s *Store) RegisterNode(ctx context.Context, n Node) (int64, error) {
	var epoch int64
	err := s.pool.QueryRow(ctx, `INSERT INTO nodes (name, addr, epoch, cpu_millis, memory_mib) VALUES ($1, $2, 1, $3, $4)
		ON CONFLICT (name) DO UPDATE SET addr = excluded.addr, epoch = nodes.epoch + 1,
			cpu_millis = excluded.cpu_millis, memory_mib = excluded.memory_mib
		RETURNING epoch`, n.Name, n.Addr, n.CPUMillis, n.MemoryMiB).Scan(&epoch)
	return epoch, err
}

func (s *Store) ListNodes(ctx context.Context) ([]Node, error) {
	rows, err := s.pool.Query(ctx, "SELECT name, addr, epoch, cpu_millis, memory_mib FROM nodes ORDER BY name")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Node, error) {
		var n Node
		err := row.Scan(&n.Name, &n.Addr, &n.Epoch, &n.CPUMillis, &n.MemoryMiB)
		return n, err
	})
}

// ReplaceInstances replaces what is known about the instances of node, if
// epoch is still the node's current one.
func (s *Store) ReplaceInstances(ctx context.Context, node string, epoch int64, instances []Instance) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var current int64
		err := tx.QueryRow(ctx, "SELECT epoch FROM nodes WHERE name = $1 FOR SHARE", node).Scan(&current)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if current != epoch {
			return ErrStaleEpoch
		}
		if _, err := tx.Exec(ctx, "DELETE FROM instances WHERE node = $1", node); err != nil {
			return err
		}
		for _, in := range instances {
			if _, err := tx.Exec(ctx, `INSERT INTO instances (id, service, node, version, generation, state, addr, seccomp, route)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
				in.ID, in.Service, node, in.Version, in.Generation, in.State, in.Addr, in.Seccomp, in.Route); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) Instances(ctx context.Context) ([]Instance, error) {
	rows, err := s.pool.Query(ctx, "SELECT id, service, node, version, generation, state, addr, seccomp, route FROM instances ORDER BY id")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Instance, error) {
		var in Instance
		err := row.Scan(&in.ID, &in.Service, &in.Node, &in.Version, &in.Generation, &in.State, &in.Addr, &in.Seccomp, &in.Route)
		return in, err
	})
}

// NodeEpoch returns the current epoch of a registered node.
func (s *Store) NodeEpoch(ctx context.Context, name string) (int64, error) {
	var epoch int64
	err := s.pool.QueryRow(ctx, "SELECT epoch FROM nodes WHERE name = $1", name).Scan(&epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return epoch, err
}
