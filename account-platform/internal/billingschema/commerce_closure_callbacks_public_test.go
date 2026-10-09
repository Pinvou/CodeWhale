package billingschema

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/commerce"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"os"
	"testing"
	"time"
)

func TestCommerceClosureCallbacksRespectSubjectFence(t *testing.T) {
	for _, name := range []string{"complete-user-wait", "retry-user-wait", "complete-after-anonymize", "retry-after-anonymize"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
			defer cancel()
			f := newStageFixture(t, ctx, os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL"))
			c := f.database(t, ctx, "account", "account_service")
			ms, e := migrations.Load("../../migrations/account")
			if e != nil {
				t.Fatal(e)
			}
			if e = accountschema.Up(ctx, c, ms); e != nil {
				t.Fatal(e)
			}
			migrations.Unlock(ctx, c)
			r, e := account.NewPostgresRepository(ctx, f.serviceURL(t, c, "account_service"))
			if e != nil {
				t.Fatal(e)
			}
			defer r.Close()
			if e = r.ConfigureCommerceProvider("closure-callback", "v1"); e != nil {
				t.Fatal(e)
			}
			p := identity.Principal{Issuer: "controlled", Subject: uuid.NewString(), SessionID: uuid.NewString(), AuthenticatedAt: time.Now().UTC()}
			me, _, e := r.Bootstrap(ctx, p)
			if e != nil {
				t.Fatal(e)
			}
			jobs, e := r.LeaseWalletProvisioningJobs(ctx, 20, time.Minute)
			if e != nil || len(jobs) != 1 {
				t.Fatal(e)
			}
			if e = r.CompleteWalletProvisioning(ctx, jobs[0]); e != nil {
				t.Fatal(e)
			}
			if _, e = c.Exec(ctx, `UPDATE users SET mobile_e164='+8613800900001',mobile_verified_at=clock_timestamp() WHERE id=$1`, me.UserID); e != nil {
				t.Fatal(e)
			}
			if e = r.EnsureCommerceProviderMappings(ctx); e != nil {
				t.Fatal(e)
			}
			job, e := r.ClaimCommerceCustomerSyncJob(ctx, "closure-callback", "v1", time.Minute)
			if e != nil || job == nil {
				t.Fatal(e)
			}
			callback := func() error {
				if name == "retry-user-wait" || name == "retry-after-anonymize" {
					return r.RetryCommerceCustomerSyncJob(ctx, *job, "CONTROLLED_RETRY", time.Now().Add(time.Second))
				}
				return r.CompleteCommerceCustomerSyncJob(ctx, *job, commerce.CustomerProvisionResult{ExternalReferenceRedacted: "***0001"})
			}
			snapshot := func() string {
				var b []byte
				if e = c.QueryRow(ctx, `SELECT jsonb_build_object('mapping',(SELECT to_jsonb(m) FROM commerce_customer_mappings m WHERE id=$1),'job',(SELECT to_jsonb(j) FROM commerce_customer_sync_jobs j WHERE id=$2),'evidence',(SELECT jsonb_agg(e ORDER BY e.id) FROM commerce_customer_sync_evidence e WHERE job_id=$2),'user',(SELECT to_jsonb(u) FROM users u WHERE id=$3))`, job.MappingID, job.ID, me.UserID).Scan(&b); e != nil {
					t.Fatal(e)
				}
				return string(b)
			}
			if name == "complete-user-wait" || name == "retry-user-wait" {
				blocker, e := pgx.Connect(ctx, f.serviceURL(t, c, "account_service"))
				if e != nil {
					t.Fatal(e)
				}
				defer blocker.Close(ctx)
				hold, e := blocker.Begin(ctx)
				if e != nil {
					t.Fatal(e)
				}
				defer hold.Rollback(ctx)
				if _, e = hold.Exec(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, me.UserID); e != nil {
					t.Fatal(e)
				}
				closing := make(chan error, 1)
				go func() { _, e := r.CreateAccountClosure(ctx, p, uuid.NewString(), "CLOSE_MY_ACCOUNT"); closing <- e }()
				wait := func(pattern string) {
					deadline := time.Now().Add(5 * time.Second)
					for time.Now().Before(deadline) {
						var n bool
						// pg_stat_activity snapshots cache inside this held transaction;
						// refresh observation so the second wait sees the later callback.
						if _, e = blocker.Exec(ctx, `SELECT pg_stat_clear_snapshot()`); e != nil {
							t.Fatal(e)
						}
						if e = blocker.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity a WHERE datname=current_database() AND pid<>pg_backend_pid() AND query LIKE $2 AND ($1=ANY(pg_blocking_pids(a.pid)) OR EXISTS(SELECT 1 FROM pg_stat_activity earlier WHERE earlier.pid=ANY(pg_blocking_pids(a.pid)) AND earlier.datname=current_database() AND $1=ANY(pg_blocking_pids(earlier.pid)))))`, blocker.PgConn().PID(), pattern).Scan(&n); e != nil {
							t.Fatal(e)
						}
						if n {
							return
						}
						time.Sleep(5 * time.Millisecond)
					}
					t.Fatal("real callback/closure did not wait on subject row", pattern)
				}
				wait("%identities identity JOIN users u%FOR UPDATE OF u%")
				done := make(chan error, 1)
				go func() { done <- callback() }()
				wait("%SELECT status,subject_lifecycle_version FROM users%FOR SHARE%")
				if e = hold.Commit(ctx); e != nil {
					t.Fatal(e)
				}
				if e = <-closing; e != nil {
					t.Fatal(e)
				}
				if e = <-done; e != nil {
					t.Fatal(e)
				}
				var status, user string
				if e = c.QueryRow(ctx, `SELECT m.status,u.status FROM commerce_customer_mappings m JOIN users u ON u.id=m.subject_id WHERE m.id=$1`, job.MappingID).Scan(&status, &user); e != nil || status != "IN_PROGRESS" || user != "CLOSING" {
					t.Fatal("late callback overwrote closure state", status, user, e)
				}
			} else {
				if _, e = r.CreateAccountClosure(ctx, p, uuid.NewString(), "CLOSE_MY_ACCOUNT"); e != nil {
					t.Fatal(e)
				}
				item, e := r.ClaimAccountClosure(ctx)
				if e != nil || item == nil {
					t.Fatal(e)
				}
				if e = r.AdvanceAccountClosureToIdentityCleanup(ctx, *item); e != nil {
					t.Fatal(e)
				}
				item, e = r.ClaimAccountClosure(ctx)
				if e != nil || item == nil {
					t.Fatal(e)
				}
				prep, e := r.PrepareAccountClosureCleanup(ctx, item)
				if e != nil {
					t.Fatal(e)
				}
				// Controlled successful external ACKs already separately covered by the C02
				// HTTP worker fixture; here exercise its real durable state transitions.
				if e = r.MarkAccountClosureIdentityDeleted(ctx, item, prep.SubjectLifecycleVersion); e != nil {
					t.Fatal(e)
				}
				if e = r.MarkAccountClosureGrantsIsolated(ctx, item, prep.SubjectLifecycleVersion); e != nil {
					t.Fatal(e)
				}
				if e = r.MarkAccountClosureReplayCleared(ctx, item, prep.SubjectLifecycleVersion); e != nil {
					t.Fatal(e)
				}
				if e = r.AnonymizeAccountClosure(ctx, item, prep.SubjectLifecycleVersion); e != nil {
					t.Fatal(e)
				}
				before := snapshot()
				if e = callback(); !errors.Is(e, account.ErrConflict) || snapshot() != before {
					t.Fatal("cancelled old callback wrote facts", e)
				}
				if j, e := r.ClaimCommerceCustomerSyncJob(ctx, "closure-callback", "v1", time.Minute); e != nil || j != nil || snapshot() != before {
					t.Fatal("anonymized subject was reclaimed", e)
				}
				if e = r.EnsureCommerceProviderMappings(ctx); e != nil || snapshot() != before {
					t.Fatal("retry enqueue resurrected anonymized mapping", e)
				}
			}
		})
	}
}
