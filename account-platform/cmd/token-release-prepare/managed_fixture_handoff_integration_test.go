package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Called by the already mandatory managed cleanup root, within its original
// context and exclusively admitted database. The gate is not a writer/DBA.
func testManagedFixtureConnectionHandoff(t *testing.T, ctx context.Context, config *pgx.ConnConfig, admin *pgx.Conn, database string) {
	if _, err := admin.Exec(ctx, "CREATE ROLE account_policy_fixture LOGIN NOINHERIT"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		clean, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := admin.Exec(clean, "DROP ROLE account_policy_fixture"); err != nil {
			t.Error("handoff gate role cleanup failed")
		}
	})
	for _, scenario := range []struct {
		name   string
		pool   bool
		cancel bool
	}{{"connection-exit", false, false}, {"connection-cancel", false, true}, {"pool-exit", true, false}, {"pool-cancel", true, true}} {
		t.Run(scenario.name, func(t *testing.T) {
			const key = 15752729
			var clients []*pgx.Conn
			var pools []*pgxpool.Pool
			var identities []managedFixtureBackend
			var gate *pgx.Conn
			t.Cleanup(func() {
				clean, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				if gate != nil {
					if _, err := gate.Exec(clean, "SELECT pg_advisory_unlock($1)", key); err != nil {
						t.Error("handoff gate cleanup failed")
					}
				}
				for _, pool := range pools {
					pool.Close()
				}
				for _, client := range clients {
					if err := client.Close(clean); err != nil {
						t.Error("handoff client cleanup failed")
					}
				}
				for _, identity := range identities {
					if err := waitManagedFixtureBackendExit(clean, admin, identity); err != nil {
						t.Error("handoff backend cleanup failed")
					}
				}
			})
			open := func(configuration *pgx.ConnConfig) *pgx.Conn {
				t.Helper()
				client, err := pgx.ConnectConfig(ctx, configuration)
				if err != nil {
					t.Fatal(err)
				}
				clients = append(clients, client)
				identity, err := managedFixtureBackendIdentity(ctx, admin, client)
				if err != nil {
					t.Fatal(err)
				}
				identities = append(identities, identity)
				return client
			}
			target := config.Copy()
			target.Database = database
			gateConfig := target.Copy()
			gateConfig.User = "account_policy_fixture"
			gate = open(gateConfig)
			executor := open(target)
			observer := open(config)
			observerPID := observer.PgConn().PID()
			observerIdentity := identities[len(identities)-1]
			request := releaseRequest{Operation: "prepare", Database: database, Approval: "fixture-approval", Maintenance: "fixture-exclusive-window", Candidates: []string{"test_credential", "test_gateway"}, Migrations: "../../migrations/token"}
			if result, err := executeRelease(ctx, executor, request); err != nil || result.Applied || result.Decision != "CHECKED" {
				t.Fatalf("nonwriter gate baseline failed: %v", err)
			}
			if _, err := gate.Exec(ctx, "SELECT pg_advisory_lock($1)", key); err != nil {
				t.Fatal(err)
			}

			var held *pgx.Conn
			var pool *pgxpool.Pool
			var borrowed *pgxpool.Conn
			borrowedNeedsRelease := false
			if scenario.pool {
				poolConfig, err := pgxpool.ParseConfig(target.ConnString())
				if err != nil {
					t.Fatal(err)
				}
				poolConfig.ConnConfig, poolConfig.MaxConns = target.Copy(), 1
				// This controlled pool alone will queue a server-side lock while
				// leaving its client idle. A liveness ping would itself wait on
				// that gate before Close; normal governance pools keep defaults.
				poolConfig.ShouldPing = func(context.Context, pgxpool.ShouldPingParams) bool { return false }
				pool, err = pgxpool.NewWithConfig(ctx, poolConfig)
				if err != nil {
					t.Fatal(err)
				}
				pools = append(pools, pool)
				borrowed, err = pool.Acquire(ctx)
				if err != nil {
					t.Fatal(err)
				}
				borrowedNeedsRelease = true
				defer func() {
					if borrowedNeedsRelease {
						borrowed.Release()
					}
				}()
				held = borrowed.Conn()
			} else {
				held = open(target)
			}
			identity, err := managedFixtureBackendIdentity(ctx, admin, held)
			if err != nil {
				t.Fatal(err)
			}
			if scenario.pool {
				identities = append(identities, identity)
			}
			held.PgConn().Frontend().Send(&pgproto3.Query{String: "SELECT pg_advisory_lock(15752729)"})
			if err := held.PgConn().Frontend().Flush(); err != nil {
				t.Fatal(err)
			}
			for {
				var waiting bool
				if err := admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND backend_start=$2 AND wait_event_type='Lock')`, identity.pid, identity.started).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("handoff backend did not reach its real lock barrier")
				case <-time.After(10 * time.Millisecond):
				}
			}
			// A reused PID with a different start is not the old identity. This
			// synthetic generation mismatch must not wait on or retire the live
			// backend; the unchanged writer guard below still sees that backend.
			stale := identity
			stale.started = stale.started.Add(-time.Microsecond)
			if err := waitManagedFixtureBackendExit(ctx, admin, stale); err != nil {
				t.Fatal(err)
			}
			if borrowed != nil {
				borrowed.Release()
				borrowedNeedsRelease = false
			}

			waiting, cancel := context.WithCancel(ctx)
			result, done := make(chan error, 1), make(chan struct{})
			go func() {
				defer close(done)
				if scenario.pool {
					result <- closeManagedFixtureSinglePool(waiting, observer, pool)
				} else {
					result <- closeManagedFixtureConnection(waiting, observer, held)
				}
			}()
			t.Cleanup(func() { cancel(); <-done })
			// Two completed, distinct server observations prove the helper is
			// polling the held backend, rather than returning after one query.
			// Elapsed sleep alone would not establish this acknowledgement.
			var firstObservation time.Time
			for {
				select {
				case err := <-result:
					t.Fatalf("handoff returned before backend retirement was observed: %v", err)
				default:
				}
				var query, state string
				var started *time.Time
				if err := admin.QueryRow(ctx, "SELECT query,state,query_start FROM pg_stat_activity WHERE pid=$1 AND backend_start=$2", observerPID, observerIdentity.started).Scan(&query, &state, &started); err != nil {
					t.Fatal(err)
				}
				if started != nil && state == "idle" && strings.Contains(query, "pinvou owned fixture backend retirement") {
					if !firstObservation.IsZero() && !started.Equal(firstObservation) {
						break
					}
					firstObservation = *started
				}
				select {
				case <-ctx.Done():
					t.Fatal("handoff observer did not acknowledge its first observation")
				case <-time.After(10 * time.Millisecond):
				}
			}
			select {
			case err := <-result:
				t.Fatalf("handoff completed while its backend was held: %v", err)
			default:
			}
			if result, err := executeRelease(ctx, executor, request); err == nil || err.Error() != "TOKEN_RELEASE_OTHER_DBA_SESSION_PRESENT" || result.Applied {
				t.Fatalf("strict writer guard ignored the live backend: %v", err)
			}
			if scenario.cancel {
				cancel()
				if err := <-result; !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled handoff was accepted: %v", err)
				}
				var stillWaiting bool
				if err := admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND backend_start=$2 AND wait_event_type='Lock')`, identity.pid, identity.started).Scan(&stillWaiting); err != nil || !stillWaiting {
					t.Fatal("handoff cancellation terminated or lost the held backend")
				}
			}
			var unlocked bool
			if err := gate.QueryRow(ctx, "SELECT pg_advisory_unlock($1)", key).Scan(&unlocked); err != nil || !unlocked {
				t.Fatal("handoff gate release failed")
			}
			if !scenario.cancel {
				if err := <-result; err != nil {
					t.Fatal(err)
				}
			}
			if err := waitManagedFixtureBackendExit(ctx, admin, identity); err != nil {
				t.Fatal(err)
			}
			if result, err := executeRelease(ctx, executor, request); err != nil || result.Applied || result.Decision != "CHECKED" {
				t.Fatalf("next real invocation did not recover after retirement: %v", err)
			}
		})
	}
	t.Run("cancelled-pool-still-closes-owned-client", func(t *testing.T) {
		target := config.Copy()
		target.Database = database
		poolConfig, err := pgxpool.ParseConfig(target.ConnString())
		if err != nil {
			t.Fatal(err)
		}
		poolConfig.ConnConfig, poolConfig.MaxConns = target.Copy(), 1
		pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()
		connection, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		needsRelease := true
		defer func() {
			if needsRelease {
				connection.Release()
			}
		}()
		identity, err := managedFixtureBackendIdentity(ctx, admin, connection.Conn())
		if err != nil {
			t.Fatal(err)
		}
		connection.Release()
		needsRelease = false
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if err := closeManagedFixtureSinglePool(cancelled, admin, pool); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled pool handoff did not refuse success: %v", err)
		}
		unexpected, err := pool.Acquire(ctx)
		if err == nil {
			unexpected.Release()
			t.Fatal("failed handoff left its owned pool usable")
		}
		if err := waitManagedFixtureBackendExit(ctx, admin, identity); err != nil {
			t.Fatal(err)
		}
	})
}
