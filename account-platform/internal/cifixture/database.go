package cifixture

// This file is test infrastructure. Product connections must not use it.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrDatabaseUnregistered = errors.New("fixture database was not created by this process")
	ErrDatabaseIdentity     = errors.New("fixture database identity changed")
	ErrDatabaseOwner        = errors.New("fixture database owner changed")
)

type databaseKey struct{ system, directory, name string }

// Database is an opaque creation lease. A stale lease cannot adopt a same-name
// replacement, even after a later test legitimately creates that name again.
type Database struct {
	key              databaseKey
	mu               sync.Mutex
	dropMu           sync.Mutex
	id               string
	oid              uint32
	owner            string
	created, removed bool
}

// Optional job-owned evidence directory; no DSN, password or query is persisted.
// Atomic files also survive a killed test process. The job retires its admitted
// PostgreSQL container as a whole, rather than guessing ownership from these logs.
func (r *Database) record(name, state string) error {
	directory := os.Getenv("PINVOU_CI_RESOURCE_LEDGER")
	if directory == "" {
		return nil
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("fixture ledger directory unavailable")
	}
	data, err := json.Marshal(struct {
		Schema int    `json:"schema"`
		Kind   string `json:"kind"`
		ID     string `json:"id"`
		Name   string `json:"name"`
		State  string `json:"state"`
		PID    int    `json:"pid"`
	}{1, "database", r.id, name, state, os.Getpid()})
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".receipt-")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	// Receipts contain only test identity/state. A non-root job coordinator
	// must be able to collect evidence written by its isolated root tools image.
	if err = file.Chmod(0644); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, filepath.Join(directory, r.id+".json"))
}

func databaseCluster(ctx context.Context, c *pgx.Conn, name string) (databaseKey, error) {
	if c == nil || name == "" || c.Config().Database == name {
		return databaseKey{}, ErrDatabaseIdentity
	}
	key := databaseKey{name: name}
	err := c.QueryRow(ctx, "SELECT system_identifier::text,current_setting('data_directory') FROM pg_control_system()").Scan(&key.system, &key.directory)
	if err != nil {
		return key, fmt.Errorf("fixture cluster observation failed: %w", err)
	}
	return key, nil
}

// CreateDatabase registers an intent before creation and captures the actual
// cluster/database OID immediately after success. It never adopts an existing DB.
// The already-admitted coordinator and the caller's original SQL/owner policy
// remain unchanged. A failed/ambiguous creation is retained as an unconfirmed
// receipt, never silently promoted to a resource we may delete.
func CreateDatabase(ctx context.Context, c *pgx.Conn, name, statement string) (*Database, error) {
	prefix := "CREATE DATABASE " + pgx.Identifier{name}.Sanitize()
	if !strings.HasPrefix(statement, prefix) || (len(statement) > len(prefix) && statement[len(prefix)] != ' ') || strings.Contains(statement, ";") {
		return nil, ErrDatabaseIdentity
	}
	key, err := databaseCluster(ctx, c, name)
	if err != nil {
		return nil, err
	}
	var exists bool
	if err = c.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)", name).Scan(&exists); err != nil {
		return nil, err
	}
	if exists {
		return nil, ErrDatabaseIdentity
	}
	receipt := &Database{id: uuid.NewString(), key: key}
	if err = receipt.record(name, "registered"); err != nil {
		return nil, err
	}
	_, err = c.Exec(ctx, statement)
	if err != nil {
		return receipt, err
	}
	receipt.mu.Lock()
	err = c.QueryRow(ctx, "SELECT oid,pg_get_userbyid(datdba) FROM pg_database WHERE datname=$1", name).Scan(&receipt.oid, &receipt.owner)
	if err != nil {
		receipt.mu.Unlock()
		return receipt, err
	}
	receipt.created = true
	receipt.mu.Unlock()
	if err = receipt.record(name, "created"); err != nil {
		return receipt, err
	}
	return receipt, nil
}

// WaitDatabaseIdle observes PostgreSQL's acknowledgement of client shutdown.
// Identity and the caller's owner policy are checked on EVERY observation. A
// connection close is not backend exit; sleep alone is not acknowledgement.
// No backend is terminated and a cancelled observation never authorizes DROP.
func WaitDatabaseIdle(ctx context.Context, c *pgx.Conn, name string, owners ...string) error {
	return waitDatabase(ctx, c, name, nil, owners...)
}

func waitDatabase(ctx context.Context, c *pgx.Conn, name string, lease *Database, owners ...string) error {
	key, err := databaseCluster(ctx, c, name)
	if err != nil {
		return err
	}
	if lease != nil && lease.key != key {
		return ErrDatabaseIdentity
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var oid uint32
		var owner string
		var sessions int
		if err := c.QueryRow(ctx, `SELECT oid,pg_get_userbyid(datdba),(SELECT count(*) FROM pg_stat_activity WHERE datname=$1) FROM pg_database WHERE datname=$1`, name).Scan(&oid, &owner, &sessions); err != nil {
			return fmt.Errorf("fixture database observation failed: %w", err)
		}
		allowed := len(owners) > 0
		if lease != nil {
			lease.mu.Lock()
			identity := lease.created && !lease.removed && lease.oid == oid
			allowed = owner == lease.owner
			lease.mu.Unlock()
			if !identity {
				return ErrDatabaseIdentity
			}
		}
		if len(owners) > 0 {
			allowed = false
			for _, expected := range owners {
				if owner == expected {
					allowed = true
				}
			}
		}
		if !allowed {
			return ErrDatabaseOwner
		}
		if sessions == 0 {
			return nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// DropDatabase is the single destructive database cleanup entry point. Callers
// close their clients first, keep their existing bounded context, and keep any
// stricter role/dependency/name fences. Repeated cleanup of a confirmed absent
// resource is idempotent, but a same-name replacement is never adopted.
func DropDatabase(ctx context.Context, c *pgx.Conn, receipt *Database, owners ...string) (pgconn.CommandTag, error) {
	if receipt == nil {
		return pgconn.CommandTag{}, ErrDatabaseUnregistered
	}
	name := receipt.key.name
	key, err := databaseCluster(ctx, c, name)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	if receipt.key != key {
		return pgconn.CommandTag{}, ErrDatabaseIdentity
	}
	receipt.dropMu.Lock()
	defer receipt.dropMu.Unlock()
	receipt.mu.Lock()
	removed := receipt.removed
	receipt.mu.Unlock()
	if removed {
		var exists bool
		if err = c.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)", name).Scan(&exists); err != nil {
			return pgconn.CommandTag{}, err
		}
		if exists {
			return pgconn.CommandTag{}, ErrDatabaseIdentity
		}
		return pgconn.NewCommandTag("DROP DATABASE"), receipt.record(name, "released")
	}
	if err = waitDatabase(ctx, c, name, receipt, owners...); err != nil {
		return pgconn.CommandTag{}, err
	}
	tag, err := c.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize())
	if err != nil {
		return tag, err
	}
	var exists bool
	if err = c.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)", name).Scan(&exists); err != nil {
		return tag, err
	}
	if exists {
		return tag, ErrDatabaseIdentity
	}
	receipt.mu.Lock()
	receipt.removed = true
	receipt.mu.Unlock()
	return tag, receipt.record(name, "released")
}
