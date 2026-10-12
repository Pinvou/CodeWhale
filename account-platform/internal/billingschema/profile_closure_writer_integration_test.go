package billingschema

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The identity ACK is a controlled persisted fact, not a live Keycloak call.
// Installation and both writers use the real public schema and runtime actor.
func TestProfileClosureWritersRespectSubjectFence(t *testing.T) {
	if os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL") == "" {
		t.Skip("explicit isolated schema database required")
	}
	for _, name := range []string{"active-commit-replay", "disabled-legacy-commit", "subject-before-profile", "prepare-takes-confirmed-retry", "cancelled-unconfirmed-no-write", "sessions-user-before-profile", "late-ack-preserves-cancellation", "completed-before-prepare", "sessions-concurrent-prepare"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
			defer cancel()
			f := newStageFixture(t, ctx, os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL"))
			c := f.database(t, ctx, "account", "account_service")
			ms, err := migrations.Load("../../migrations/account")
			if err != nil {
				t.Fatal(err)
			}
			if err = accountschema.Up(ctx, c, ms); err != nil {
				t.Fatal(err)
			}
			migrations.Unlock(ctx, c)
			r, err := account.NewPostgresRepository(ctx, f.serviceURL(t, c, "account_service"))
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			p := identity.Principal{Issuer: "controlled", Subject: uuid.NewString(), SessionID: uuid.NewString(), AuthenticatedAt: time.Now().UTC()}
			me, _, err := r.Bootstrap(ctx, p)
			if err != nil {
				t.Fatal(err)
			}
			jobs, err := r.LeaseWalletProvisioningJobs(ctx, 20, time.Minute)
			if err != nil || len(jobs) != 1 {
				t.Fatal(err)
			}
			if err = r.CompleteWalletProvisioning(ctx, jobs[0]); err != nil {
				t.Fatal(err)
			}
			id := uuid.New()
			confirmed := name != "cancelled-unconfirmed-no-write"
			if _, err = c.Exec(ctx, `INSERT INTO profile_change_requests(id,user_id,identity_issuer,identity_subject,field,old_value,new_value,expected_profile_version,identity_updated_at,status,next_attempt_at,sla_expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,'USERNAME','','controlled-new-name',1,CASE WHEN $5 THEN clock_timestamp() END,CASE WHEN $5 THEN 'IDENTITY_UPDATED' ELSE 'VERIFIED' END,clock_timestamp(),clock_timestamp()+interval '1 hour',clock_timestamp(),clock_timestamp())`, id, me.UserID, p.Issuer, p.Subject, confirmed); err != nil {
				t.Fatal(err)
			}
			if name == "disabled-legacy-commit" {
				if _, err = c.Exec(ctx, `UPDATE users SET status='DISABLED' WHERE id=$1`, me.UserID); err != nil {
					t.Fatal(err)
				}
			}
			wait := func(fragment string) {
				t.Helper()
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					var n int
					if _, err = c.Exec(ctx, `SELECT pg_stat_clear_snapshot()`); err != nil {
						t.Fatal(err)
					}
					if err = c.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND state='active' AND cardinality(pg_blocking_pids(pid))>0 AND query LIKE '%'||$1||'%'`, fragment).Scan(&n); err != nil {
						t.Fatal(err)
					}
					if n > 0 {
						return
					}
					time.Sleep(5 * time.Millisecond)
				}
				t.Fatal("writer did not wait on the expected real row lock", fragment)
			}
			if name == "subject-before-profile" || name == "sessions-user-before-profile" {
				if name == "sessions-user-before-profile" {
					if err = r.CommitProfileChange(ctx, id); err != nil {
						t.Fatal(err)
					}
				}
				gate, err := c.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer gate.Rollback(ctx)
				if _, err = gate.Exec(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, me.UserID); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() {
					if name == "sessions-user-before-profile" {
						done <- r.MarkProfileSessionsRevoked(ctx, id)
					} else {
						done <- r.CommitProfileChange(ctx, id)
					}
				}()
				if name == "sessions-user-before-profile" {
					wait("SELECT id FROM users")
				} else {
					wait("SELECT status FROM users")
				}
				probe, err := pgx.Connect(ctx, f.serviceURL(t, c, "account_service"))
				if err != nil {
					t.Fatal(err)
				}
				defer probe.Close(ctx)
				tx, err := probe.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(ctx)
				if _, err = tx.Exec(ctx, `SELECT id FROM profile_change_requests WHERE id=$1 FOR UPDATE NOWAIT`, id); err != nil {
					t.Fatal("waiting writer locked profile before user", err)
				}
				if err = tx.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
				if err = gate.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				if err = <-done; err != nil {
					t.Fatal(err)
				}
				if name == "sessions-user-before-profile" {
					var before, after string
					if err = c.QueryRow(ctx, `SELECT to_jsonb(p)::text FROM profile_change_requests p WHERE id=$1 AND sessions_revoked_at IS NOT NULL`, id).Scan(&before); err != nil {
						t.Fatal(err)
					}
					if err = r.MarkProfileSessionsRevoked(ctx, id); err != nil {
						t.Fatal(err)
					}
					if err = c.QueryRow(ctx, `SELECT to_jsonb(p)::text FROM profile_change_requests p WHERE id=$1`, id).Scan(&after); err != nil || before != after {
						t.Fatal("revocation replay changed facts", err)
					}
					return
				}
			} else if name == "late-ack-preserves-cancellation" || name == "completed-before-prepare" || name == "sessions-concurrent-prepare" {
				if err = r.CommitProfileChange(ctx, id); err != nil {
					t.Fatal(err)
				}
				if name == "completed-before-prepare" {
					if err = r.MarkProfileSessionsRevoked(ctx, id); err != nil {
						t.Fatal(err)
					}
					if err = r.CompleteProfileChange(ctx, id); err != nil {
						t.Fatal(err)
					}
				}
				if _, err = r.CreateAccountClosure(ctx, p, uuid.NewString(), "CLOSE_MY_ACCOUNT"); err != nil {
					t.Fatal(err)
				}
				item, err := r.ClaimAccountClosure(ctx)
				if err != nil || item == nil {
					t.Fatal(err)
				}
				if err = r.AdvanceAccountClosureToIdentityCleanup(ctx, *item); err != nil {
					t.Fatal(err)
				}
				item, err = r.ClaimAccountClosure(ctx)
				if err != nil || item == nil {
					t.Fatal(err)
				}
				if name == "sessions-concurrent-prepare" {
					gate, err := c.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer gate.Rollback(ctx)
					if _, err = gate.Exec(ctx, `SELECT id FROM profile_change_requests WHERE id=$1 FOR UPDATE`, id); err != nil {
						t.Fatal(err)
					}
					marked := make(chan error, 1)
					go func() { marked <- r.MarkProfileSessionsRevoked(ctx, id) }()
					wait("FROM profile_change_requests WHERE id=")
					prepared := make(chan error, 1)
					go func() { _, e := r.PrepareAccountClosureCleanup(ctx, item); prepared <- e }()
					wait("FOR UPDATE OF request,users")
					if err = gate.Commit(ctx); err != nil {
						t.Fatal(err)
					}
					if err = <-marked; err != nil {
						t.Fatal(err)
					}
					err = <-prepared
					retries := 0
					var pe *pgconn.PgError
					if errors.As(err, &pe) && pe.Code == "40001" {
						// Prepare has returned and rolled back its first transaction.
						// Retain its original claim; no new lease or authority is issued.
						var live bool
						if e := c.QueryRow(ctx, `SELECT version=$2 AND lease_until>clock_timestamp() AND stage='IDENTITY_CLEANUP' AND status='IN_PROGRESS' FROM account_closure_requests WHERE id=$1`, item.ID, item.Version).Scan(&live); e != nil || !live {
							t.Fatal("same claim no longer live", e)
						}
						retries = 1
						_, err = r.PrepareAccountClosureCleanup(ctx, item)
					}
					t.Logf("same_claim_serialization_retries=%d", retries)
					if err != nil {
						t.Fatal(err)
					}
				} else if _, err = r.PrepareAccountClosureCleanup(ctx, item); err != nil {
					t.Fatal(err)
				}
				var userBefore, userAfter string
				if err = c.QueryRow(ctx, `SELECT to_jsonb(u)::text FROM users u WHERE id=$1`, me.UserID).Scan(&userBefore); err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 2; i++ {
					if err = r.MarkProfileSessionsRevoked(ctx, id); err != nil {
						t.Fatal(err)
					}
					if err = r.CompleteProfileChange(ctx, id); err != nil {
						t.Fatal(err)
					}
				}
				var status, reason string
				var ack bool
				if err = c.QueryRow(ctx, `SELECT status,COALESCE(last_error_code,''),sessions_revoked_at IS NOT NULL FROM profile_change_requests WHERE id=$1`, id).Scan(&status, &reason, &ack); err != nil {
					t.Fatal(err)
				}
				if name == "late-ack-preserves-cancellation" || name == "sessions-concurrent-prepare" {
					if status != "CANCELLED_ACCOUNT_CLOSURE" || reason != "ACCOUNT_CLOSURE" || !ack {
						t.Fatal("late ACK overwrote cancellation evidence", status, reason, ack)
					}
				} else if status != "COMPLETED" || reason != "" || !ack {
					t.Fatal("completed receipt changed", status, reason, ack)
				}
				if err = c.QueryRow(ctx, `SELECT to_jsonb(u)::text FROM users u WHERE id=$1`, me.UserID).Scan(&userAfter); err != nil || userBefore != userAfter {
					t.Fatal("late ACK mutated subject", err)
				}
				return
			} else if name == "prepare-takes-confirmed-retry" || name == "cancelled-unconfirmed-no-write" {
				if _, err = r.CreateAccountClosure(ctx, p, uuid.NewString(), "CLOSE_MY_ACCOUNT"); err != nil {
					t.Fatal(err)
				}
				item, err := r.ClaimAccountClosure(ctx)
				if err != nil || item == nil {
					t.Fatal(err)
				}
				if err = r.AdvanceAccountClosureToIdentityCleanup(ctx, *item); err != nil {
					t.Fatal(err)
				}
				item, err = r.ClaimAccountClosure(ctx)
				if err != nil || item == nil {
					t.Fatal(err)
				}
				if err = r.CommitProfileChange(ctx, id); !errors.Is(err, account.ErrConflict) {
					t.Fatal("closing writer should retain the pending evidence", err)
				}
				if confirmed {
					if err = r.ScheduleProfileChangeRetry(ctx, id, "ACCOUNT_COMMIT_FAILED"); err != nil {
						t.Fatal(err)
					}
					var preserved bool
					if err = c.QueryRow(ctx, `SELECT status='RETRY' AND identity_updated_at IS NOT NULL AND account_committed_at IS NULL FROM profile_change_requests WHERE id=$1`, id).Scan(&preserved); err != nil || !preserved {
						t.Fatal("retry discarded identity ACK", err)
					}
				}
				gate, err := c.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer gate.Rollback(ctx)
				if _, err = gate.Exec(ctx, `SELECT id FROM profile_change_requests WHERE id=$1 FOR UPDATE`, id); err != nil {
					t.Fatal(err)
				}
				prepared := make(chan error, 1)
				go func() { _, e := r.PrepareAccountClosureCleanup(ctx, item); prepared <- e }()
				wait("FROM profile_change_requests WHERE user_id=")
				committed := make(chan error, 1)
				go func() { committed <- r.CommitProfileChange(ctx, id) }()
				wait("SELECT status FROM users")
				if err = gate.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				if err = <-prepared; err != nil {
					t.Fatal(err)
				}
				err = <-committed
				var pe *pgconn.PgError
				if err != nil && !errors.Is(err, account.ErrConflict) && !(errors.As(err, &pe) && pe.Code == "40001") {
					t.Fatal("unexpected concurrent writer result", err)
				}
				var got, user string
				var hasACK, hasCommit bool
				if err = c.QueryRow(ctx, `SELECT p.status,p.identity_updated_at IS NOT NULL,p.account_committed_at IS NOT NULL,COALESCE(u.username,'') FROM profile_change_requests p JOIN users u ON u.id=p.user_id WHERE p.id=$1`, id).Scan(&got, &hasACK, &hasCommit, &user); err != nil || got != "CANCELLED_ACCOUNT_CLOSURE" || hasACK != confirmed || hasCommit != confirmed {
					t.Fatal("cleanup did not preserve the actual ACK", got, hasACK, hasCommit, err)
				}
				if confirmed && user != "controlled-new-name" || !confirmed && user != "" {
					t.Fatal("incorrect identity reconciliation", user)
				}
				if err = r.CommitProfileChange(ctx, id); confirmed && err != nil || !confirmed && !errors.Is(err, account.ErrConflict) {
					t.Fatal("same request replay changed the result", err)
				}
				return
			} else if err = r.CommitProfileChange(ctx, id); err != nil {
				t.Fatal(err)
			}
			var before, after string
			if err = c.QueryRow(ctx, `SELECT jsonb_build_object('user',to_jsonb(u),'profile',to_jsonb(p))::text FROM users u JOIN profile_change_requests p ON p.user_id=u.id WHERE p.id=$1`, id).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if err = r.CommitProfileChange(ctx, id); err != nil {
				t.Fatal(err)
			}
			if err = c.QueryRow(ctx, `SELECT jsonb_build_object('user',to_jsonb(u),'profile',to_jsonb(p))::text FROM users u JOIN profile_change_requests p ON p.user_id=u.id WHERE p.id=$1`, id).Scan(&after); err != nil || before != after {
				t.Fatal("committed replay mutated stored facts", err)
			}
		})
	}
}
