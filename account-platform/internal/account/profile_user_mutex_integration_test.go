package account

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
					t.Fatal("formal producer did not wait on USER", fragment)
				}
				committed := make(chan error, 1)
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
				created := make(chan error, 1)
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
