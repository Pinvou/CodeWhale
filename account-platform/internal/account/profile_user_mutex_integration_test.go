package account

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestF01ProfileCommitUsesUserMutex(t *testing.T) {
	f := retainedWalletFenceActors(t)
	r := f.repository(t)
	ctx := f.ctx
	for _, name := range []string{"active-commit-replay", "disabled-existing-commit", "create-commit-share-user-mutex", "unconfirmed-remains-conflict"} {
		t.Run(name, func(t *testing.T) {
			principal, user := f01ActiveFixtureUser(t, r)
			value := "mutex" + strings.ReplaceAll(uuid.NewString(), "-", "")[:15]
			change, err := r.CreateProfileChange(ctx, principal, "USERNAME", value)
			if err != nil {
				t.Fatal(err)
			}
			if name == "unconfirmed-remains-conflict" {
				if err = r.CommitProfileChange(ctx, change.ID); !errors.Is(err, ErrConflict) {
					t.Fatal("unconfirmed identity was committed", err)
				}
				var committed bool
				if err = r.pool.QueryRow(ctx, `SELECT account_committed_at IS NOT NULL FROM profile_change_requests WHERE id=$1`, change.ID).Scan(&committed); err != nil || committed {
					t.Fatal("unconfirmed receipt changed", err)
				}
				return
			}
			// This is the persisted identity adapter ACK boundary, not a live
			// external identity-provider invocation.
			if err = r.MarkProfileIdentityUpdated(ctx, change.ID); err != nil {
				t.Fatal(err)
			}
			if name == "disabled-existing-commit" {
				if _, err = r.pool.Exec(ctx, `UPDATE users SET status='DISABLED' WHERE id=$1`, user); err != nil {
					t.Fatal(err)
				}
			}
			if name == "create-commit-share-user-mutex" {
				// Establish the three genuine runtime connections needed by Commit,
				// Create and their observer inside the original fixture setup budget.
				// Hold them together so Acquire cannot repeatedly reuse one socket;
				// release every connection before the unchanged five-second lock test.
				var readyConnections []*pgxpool.Conn
				defer func() {
					for _, connection := range readyConnections {
						connection.Release()
					}
				}()
				for range 3 {
					connection, err := r.pool.Acquire(ctx)
					if err != nil {
						t.Fatal("runtime connection setup failed")
					}
					readyConnections = append(readyConnections, connection)
					var database, actor string
					var super bool
					if err = connection.QueryRow(ctx, `SELECT current_database(),current_user,(SELECT rolsuper FROM pg_roles WHERE rolname=current_user)`).Scan(&database, &actor, &super); err != nil || database != r.pool.Config().ConnConfig.Database || actor != "account_service" || super {
						t.Fatal("runtime connection setup identity not proven")
					}
				}
				for _, connection := range readyConnections {
					connection.Release()
				}
				readyConnections = nil
				gate, err := pgx.ConnectConfig(ctx, r.pool.Config().ConnConfig.Copy())
				if err != nil {
					t.Fatal(err)
				}
				defer gate.Close(context.Background())
				hold, err := gate.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer hold.Rollback(context.Background())
				if _, err = hold.Exec(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, user); err != nil {
					t.Fatal(err)
				}
				committed := make(chan error, 1)
				created := make(chan error, 1)
				wait := func(fragment string) {
					t.Helper()
					deadline := time.Now().Add(5 * time.Second)
					for time.Now().Before(deadline) {
						var n int
						if e := r.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND state='active' AND cardinality(pg_blocking_pids(pid))>0 AND query LIKE '%'||$1||'%'`, fragment).Scan(&n); e != nil {
							t.Fatal(e)
						}
						if n > 0 {
							return
						}
						time.Sleep(5 * time.Millisecond)
					}
					// Failure-only diagnostics preserve the original observation budget
					// and assertions. The first intermittent failure is not explained
					// by a later successful run or by these observations alone.
					classify := func(e error) string {
						if e == nil {
							return "SUCCESS"
						}
						if errors.Is(e, ErrProfileChangeConflict) {
							return "PROFILE_CONFLICT"
						}
						if errors.Is(e, ErrNotFound) {
							return "NOT_FOUND"
						}
						if errors.Is(e, context.Canceled) {
							return "CANCELLED"
						}
						if errors.Is(e, context.DeadlineExceeded) {
							return "DEADLINE"
						}
						var databaseError interface{ SQLState() string }
						if errors.As(e, &databaseError) {
							code := databaseError.SQLState()
							if len(code) == 5 && strings.Trim(code, "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ") == "" {
								return "PG_" + code
							}
						}
						return "OTHER"
					}
					returned := func(result <-chan error) string {
						select {
						case e := <-result:
							return classify(e)
						default:
							return "NOT_RETURNED"
						}
					}
					commitResult, createResult := returned(committed), returned(created)
					stats := r.pool.Stat()
					diagnosticCtx, diagnosticCancel := context.WithTimeout(ctx, time.Second)
					defer diagnosticCancel()
					var queryStates string
					diagnosticErr := r.pool.QueryRow(diagnosticCtx, `SELECT COALESCE(string_agg(CASE WHEN query LIKE '%FROM users u JOIN identities i%' THEN 'CREATE_USER' WHEN query LIKE 'SELECT id FROM users%' THEN 'COMMIT_USER' WHEN query LIKE '%FROM profile_change_requests%' THEN 'PROFILE' ELSE 'OTHER' END || ':' || COALESCE(wait_event_type,'NONE') || ':' || COALESCE(wait_event,'NONE') || ':' || cardinality(pg_blocking_pids(pid))::text,','),'EMPTY') FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND state='active'`).Scan(&queryStates)
					phase := "COMMIT_USER"
					if fragment == "FROM users u JOIN identities i" {
						phase = "CREATE_USER"
					}
					// Consumed results cannot reach the normal receive path: Fatal
					// exits this subtest and its deferred gate rollback releases peers.
					t.Fatalf("formal producer did not wait on USER: phase=%s commit=%s create=%s diagnostic=%s states=%s pool_total=%d pool_acquired=%d pool_idle=%d pool_constructing=%d pool_cancelled_acquires=%d", phase, commitResult, createResult, classify(diagnosticErr), queryStates, stats.TotalConns(), stats.AcquiredConns(), stats.IdleConns(), stats.ConstructingConns(), stats.CanceledAcquireCount())
				}
				go func() { committed <- r.CommitProfileChange(ctx, change.ID) }()
				wait("SELECT id FROM users WHERE id=")
				probe, err := r.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer probe.Rollback(context.Background())
				if _, err = probe.Exec(ctx, `SELECT id FROM profile_change_requests WHERE id=$1 FOR UPDATE NOWAIT`, change.ID); err != nil {
					t.Fatal("Commit held profile while waiting for USER", err)
				}
				if err = probe.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
				go func() {
					_, e := r.CreateProfileChange(ctx, principal, "USERNAME", "next"+strings.ReplaceAll(uuid.NewString(), "-", "")[:15])
					created <- e
				}()
				wait("FROM users u JOIN identities i")
				if err = hold.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				if err = <-committed; err != nil {
					t.Fatal("confirmed commit failed", err)
				}
				if err = <-created; !errors.Is(err, ErrProfileChangeConflict) {
					t.Fatal("second producer did not observe committed facts", err)
				}
			} else if err = r.CommitProfileChange(ctx, change.ID); err != nil {
				t.Fatal(err)
			}
			var username string
			var hasIdentity, hasCommit bool
			if err = r.pool.QueryRow(ctx, `SELECT u.username,p.identity_updated_at IS NOT NULL,p.account_committed_at IS NOT NULL FROM users u JOIN profile_change_requests p ON p.user_id=u.id WHERE p.id=$1`, change.ID).Scan(&username, &hasIdentity, &hasCommit); err != nil || username != value || !hasIdentity || !hasCommit {
				t.Fatal("identity receipt or profile changed", err)
			}
			var before, after string
			query := `SELECT jsonb_build_object('user',to_jsonb(u),'profile',to_jsonb(p))::text FROM users u JOIN profile_change_requests p ON p.user_id=u.id WHERE p.id=$1`
			if err = r.pool.QueryRow(ctx, query, change.ID).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if err = r.CommitProfileChange(ctx, change.ID); err != nil {
				t.Fatal(err)
			}
			if err = r.pool.QueryRow(ctx, query, change.ID).Scan(&after); err != nil || before != after {
				t.Fatal("committed replay wrote facts", err)
			}
		})
	}
}
