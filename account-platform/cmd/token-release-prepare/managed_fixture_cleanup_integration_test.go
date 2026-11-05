package main

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/cifixture"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
)

// Closing a client socket does not acknowledge PostgreSQL backend exit. Wait
// within the caller's existing cleanup budget; never force a drop or terminate
// a backend. Ownership must remain valid on every observation, and DROP DATABASE
// still performs PostgreSQL's final no-live-sessions check after this barrier.
func waitManagedFixtureDatabaseIdle(ctx context.Context, admin *pgx.Conn, database string) error {
	return cifixture.WaitDatabaseIdle(ctx, admin, database, "token_gateway", "token_schema_owner")
}

func TestManagedFixtureDatabaseCleanupPostgres(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_MANAGED_POSTGRES_URL")
	if raw == "" {
		t.Skip("dedicated TLS managed-control fixture is not configured")
	}
	config, err := pgx.ParseConfig(raw)
	if err != nil || config.Database != "managed_control_fixture" || config.User != "pinvou_control_fixture" || config.Host != "127.0.0.1" {
		t.Fatal("refusing a database outside the dedicated local managed-control fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	var exclusive bool
	if admin.QueryRow(ctx, `SELECT current_setting('data_directory')='/var/lib/postgresql/data/managed_control_fixture'
 AND NOT EXISTS(SELECT 1 FROM pg_database WHERE NOT datistemplate AND datname NOT IN ('postgres','managed_control_fixture'))
 AND NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname !~ '^pg_' AND rolname<>session_user)`).Scan(&exclusive) != nil || !exclusive {
		t.Fatal("refusing writes outside the newly initialized exclusive fixture cluster")
	}
	database := "managed_cleanup_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	var fixtureDatabase *cifixture.Database
	if _, err := admin.Exec(ctx, "CREATE ROLE token_gateway NOLOGIN INHERIT"); err != nil {
		t.Fatal(err)
	}
	created := false
	defer func() {
		clean, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		if created {
			if err := waitManagedFixtureDatabaseIdle(clean, admin, database); err != nil {
				t.Errorf("probe cleanup guard refused: %v", err)
				return
			}
			if _, err := cifixture.DropDatabase(clean, admin, fixtureDatabase, "token_gateway", "token_schema_owner"); err != nil {
				t.Error(err)
				return
			}
		}
		if _, err := admin.Exec(clean, "DROP ROLE token_gateway"); err != nil {
			t.Error(err)
		}
	}()
	fixtureDatabaseCreated, creationError := cifixture.CreateDatabase(ctx, admin, database, "CREATE DATABASE "+pgx.Identifier{database}.Sanitize()+" OWNER token_gateway")
	fixtureDatabase = fixtureDatabaseCreated
	if err := creationError; err != nil {
		t.Fatal(err)
	}
	created = true
	probeConfig := config.Copy()
	probeConfig.Database = database
	open := func(t *testing.T) *pgx.Conn {
		t.Helper()
		conn, err := pgx.ConnectConfig(ctx, probeConfig)
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}
	startWaiter := func(t *testing.T) <-chan error {
		t.Helper()
		observer, err := pgx.ConnectConfig(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		waiting, stop := context.WithCancel(ctx)
		result, done := make(chan error, 1), make(chan struct{})
		go func() {
			defer close(done)
			result <- waitManagedFixtureDatabaseIdle(waiting, observer, database)
		}()
		t.Cleanup(func() {
			stop()
			<-done
			observer.Close(context.Background())
		})
		return result
	}
	t.Run("client-close-precedes-backend-exit", func(t *testing.T) {
		gate, held := open(t), open(t)
		defer gate.Close(context.Background())
		defer held.Close(context.Background())
		const key = 15752268
		if _, err := gate.Exec(ctx, "SELECT pg_advisory_lock($1)", key); err != nil {
			t.Fatal(err)
		}
		// Send a real query without waiting for its result. The other fixture
		// connection controls when this backend may consume Terminate.
		held.PgConn().Frontend().Send(&pgproto3.Query{String: "SELECT pg_advisory_lock(15752268)"})
		if err := held.PgConn().Frontend().Flush(); err != nil {
			t.Fatal(err)
		}
		for {
			var waiting bool
			if err := admin.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock')", held.PgConn().PID()).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("backend did not reach the controlled lock barrier")
			case <-time.After(10 * time.Millisecond):
			}
		}
		pid := held.PgConn().PID()
		if err := held.Close(ctx); err != nil {
			t.Fatal(err)
		}
		var stillPresent bool
		if err := admin.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND datname=$2)", pid, database).Scan(&stillPresent); err != nil || !stillPresent || !held.IsClosed() {
			t.Fatal("controlled client-close/backend-exit gap was not established")
		}
		result := startWaiter(t)
		select {
		case err := <-result:
			t.Fatalf("cleanup completed before the controlled backend exited: %v", err)
		case <-time.After(30 * time.Millisecond):
		}
		if _, err := gate.Exec(ctx, "SELECT pg_advisory_unlock($1)", key); err != nil {
			t.Fatal(err)
		}
		if err := gate.Close(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-result:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("backend exit was not acknowledged within the cleanup budget")
		}
	})
	t.Run("live-backend-timeout-does-not-terminate", func(t *testing.T) {
		held := open(t)
		defer held.Close(context.Background())
		observer, err := pgx.ConnectConfig(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		defer observer.Close(context.Background())
		short, stop := context.WithTimeout(ctx, 40*time.Millisecond)
		defer stop()
		if err := waitManagedFixtureDatabaseIdle(short, observer, database); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("active backend was not refused at the deadline: %v", err)
		}
		var one int
		if err := held.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
			t.Fatal("cleanup terminated the live fixture backend")
		}
	})
	t.Run("unexpected-owner-refused", func(t *testing.T) {
		if err := waitManagedFixtureDatabaseIdle(ctx, admin, config.Database); err == nil || (!errors.Is(err, cifixture.ErrDatabaseOwner) && !errors.Is(err, cifixture.ErrDatabaseIdentity)) {
			t.Fatalf("unexpected database owner was accepted: %v", err)
		}
	})
	t.Run("owner-change-during-wait-refused", func(t *testing.T) {
		held := open(t)
		defer held.Close(context.Background())
		controller, err := pgx.ConnectConfig(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		defer controller.Close(context.Background())
		changed := false
		defer func() {
			if changed {
				if _, err := controller.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{database}.Sanitize()+" OWNER TO token_gateway"); err != nil {
					t.Error(err)
				}
			}
		}()
		result := startWaiter(t)
		select {
		case err := <-result:
			t.Fatalf("cleanup completed while a fixture backend was still live: %v", err)
		case <-time.After(30 * time.Millisecond):
		}
		if _, err := controller.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{database}.Sanitize()+" OWNER TO pinvou_control_fixture"); err != nil {
			t.Fatal(err)
		}
		changed = true
		select {
		case err := <-result:
			if !errors.Is(err, cifixture.ErrDatabaseOwner) {
				t.Fatalf("owner change during cleanup was accepted: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("cleanup did not recheck ownership while waiting")
		}
	})
	t.Run("missing-database-refused", func(t *testing.T) {
		if err := waitManagedFixtureDatabaseIdle(ctx, admin, database+"_absent"); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("missing database identity was accepted: %v", err)
		}
	})
	t.Run("creation-lease-is-idempotent-and-refuses-same-name-replacement", func(t *testing.T) {
		name := database + "_lease"
		create := func() *cifixture.Database {
			t.Helper()
			lease, err := cifixture.CreateDatabase(ctx, admin, name, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" OWNER token_gateway")
			if err != nil {
				t.Fatal(err)
			}
			return lease
		}
		first := create()
		if _, err := cifixture.DropDatabase(ctx, admin, first, "token_gateway"); err != nil {
			t.Fatal(err)
		}
		if _, err := cifixture.DropDatabase(ctx, admin, first, "token_gateway"); err != nil {
			t.Fatal("repeated cleanup is not idempotent", err)
		}
		second := create()
		defer func() {
			if _, err := cifixture.DropDatabase(ctx, admin, second, "token_gateway"); err != nil {
				t.Error(err)
			}
		}()
		if _, err := cifixture.DropDatabase(ctx, admin, first, "token_gateway"); !errors.Is(err, cifixture.ErrDatabaseIdentity) {
			t.Fatal("stale lease adopted the next invocation", err)
		}
		var exists bool
		if err := admin.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)", name).Scan(&exists); err != nil || !exists {
			t.Fatal("replacement database was not preserved", err)
		}
	})
	t.Run("nil-lease-never-authorizes-a-drop", func(t *testing.T) {
		if _, err := cifixture.DropDatabase(ctx, admin, nil); !errors.Is(err, cifixture.ErrDatabaseUnregistered) {
			t.Fatal("unowned cleanup was accepted", err)
		}
	})
	t.Run("partial-setup-retains-primary-error-and-retires-created-database", func(t *testing.T) {
		name := database + "_partial"
		primary := errors.New("controlled setup failure")
		setup := func() error {
			lease, err := cifixture.CreateDatabase(ctx, admin, name, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" OWNER token_gateway")
			if err != nil {
				return err
			}
			defer func() {
				if _, err := cifixture.DropDatabase(ctx, admin, lease, "token_gateway"); err != nil {
					t.Error(err)
				}
			}()
			return primary
		}
		if setup() != primary {
			t.Fatal("cleanup replaced the primary failure")
		}
		var exists bool
		if err := admin.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)", name).Scan(&exists); err != nil || exists {
			t.Fatal("partial setup left an owned database", err)
		}
	})
}
