package billingschema

import (
	"context"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/commerce"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"os"
	"testing"
	"time"
)

func TestF02CommerceSupersededGenerationCannotDispatch(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("explicit isolated PostgreSQL fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	c := f.database(t, ctx, "account", "account_service")
	all, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if err = accountschema.Up(ctx, c, all); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, c)
	r, err := account.NewPostgresRepository(ctx, f.serviceURL(t, c, "account_service"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	if err = r.ConfigureCommerceProvider("controlled-commerce", "v1"); err != nil {
		t.Fatal(err)
	}
	for index, mode := range []string{"inflight-complete", "inflight-retry", "expired-lease", "confirmed-mobile-wins-lock"} {
		t.Run(mode, func(t *testing.T) {
			p := identity.Principal{Issuer: "https://owned.invalid", Subject: uuid.NewString(), SessionID: uuid.NewString()}
			u, _, e := r.Bootstrap(ctx, p)
			if e != nil {
				t.Fatal(e)
			}
			jobs, e := r.LeaseWalletProvisioningJobs(ctx, 20, time.Minute)
			if e != nil || len(jobs) != 1 {
				t.Fatal("wallet job", e)
			}
			if e = r.CompleteWalletProvisioning(ctx, jobs[0]); e != nil {
				t.Fatal(e)
			}
			if _, e = c.Exec(ctx, `UPDATE users SET mobile_e164=$2,mobile_verified_at=now() WHERE id=$1`, u.UserID, fmt.Sprintf("+86138000012%02d", 70+index*2)); e != nil {
				t.Fatal(e)
			}
			if e = r.EnsureCommerceProviderMappings(ctx); e != nil {
				t.Fatal(e)
			}
			old, e := r.ClaimCommerceCustomerSyncJob(ctx, "controlled-commerce", "v1", time.Minute)
			if e != nil || old == nil || old.SubjectID != u.UserID {
				t.Fatal("original live dispatch", e)
			}
			// The formal, externally-confirmed MOBILE producer supersedes this same adapter.
			change, e := r.CreateProfileChange(ctx, p, "MOBILE", fmt.Sprintf("+86138000012%02d", 71+index*2))
			if e != nil {
				t.Fatal(e)
			}
			if e = r.MarkProfileIdentityUpdated(ctx, change.ID); e != nil {
				t.Fatal(e)
			}
			if mode == "confirmed-mobile-wins-lock" {
				if _, e = c.Exec(ctx, `UPDATE commerce_customer_sync_jobs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, old.ID); e != nil {
					t.Fatal(e)
				}
				gate, e := c.Begin(ctx)
				if e != nil {
					t.Fatal(e)
				}
				defer gate.Rollback(ctx)
				if _, e = gate.Exec(ctx, `SELECT id FROM commerce_customer_mappings WHERE id=$1 FOR UPDATE`, old.MappingID); e != nil {
					t.Fatal(e)
				}
				committed := make(chan error, 1)
				go func() { committed <- r.CommitProfileChange(ctx, change.ID) }()
				waitBlocked := func(class string) {
					t.Helper()
					deadline := time.Now().Add(5 * time.Second)
					for {
						// Observation shares the gate transaction; refresh its activity
						// snapshot instead of repeatedly reading the first cached view.
						if _, e := c.Exec(ctx, `SELECT pg_stat_clear_snapshot()`); e != nil {
							t.Fatal(e)
						}
						var n int
						e := c.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND state='active' AND cardinality(pg_blocking_pids(pid))>0 AND query LIKE $1`, class).Scan(&n)
						if e != nil {
							t.Fatal(e)
						}
						if n > 0 {
							return
						}
						if time.Now().After(deadline) {
							t.Fatal("formal writer wait not observed")
						}
						time.Sleep(time.Millisecond)
					}
				}
				waitBlocked("%INSERT INTO commerce_customer_mappings%")
				type claimResult struct {
					job *account.CommerceCustomerSyncJob
					err error
				}
				claimed := make(chan claimResult, 1)
				go func() {
					j, e := r.ClaimCommerceCustomerSyncJob(ctx, "controlled-commerce", "v1", time.Minute)
					claimed <- claimResult{j, e}
				}()
				waitBlocked("%FROM users%FOR SHARE%")
				if e = gate.Commit(ctx); e != nil {
					t.Fatal(e)
				}
				if e = <-committed; e != nil {
					t.Fatal(e)
				}
				result := <-claimed
				if result.err != nil || result.job != nil {
					t.Fatal("candidate escaped lock-time generation recheck", result.err)
				}
			} else if e = r.CommitProfileChange(ctx, change.ID); e != nil {
				t.Fatal(e)
			}
			before, e := r.GetCommerceCustomerMapping(ctx, p)
			if e != nil {
				t.Fatal(e)
			}
			switch mode {
			case "inflight-complete":
				if e = r.CompleteCommerceCustomerSyncJob(ctx, *old, commerce.CustomerProvisionResult{ExternalReferenceRedacted: "***obsolete"}); e != nil {
					t.Fatal("live old callback", e)
				}
				var count int
				if e = c.QueryRow(ctx, `SELECT count(*) FROM commerce_customer_sync_evidence WHERE job_id=$1 AND outcome='STALE'`, old.ID).Scan(&count); e != nil || count != 1 {
					t.Fatal("old STALE fact", e)
				}
			case "inflight-retry":
				if e = r.RetryCommerceCustomerSyncJob(ctx, *old, "COMMERCE_PROVIDER_UNAVAILABLE", time.Now().Add(-time.Hour)); e != nil {
					t.Fatal(e)
				}
			case "expired-lease":
				if _, e = c.Exec(ctx, `UPDATE commerce_customer_sync_jobs SET lease_until=clock_timestamp()-interval '1 second',next_attempt_at=clock_timestamp()-interval '1 hour' WHERE id=$1`, old.ID); e != nil {
					t.Fatal(e)
				}
			}
			after, e := r.GetCommerceCustomerMapping(ctx, p)
			if e != nil || before != after {
				t.Fatal("old callback changed current mapping", e)
			}
			current, e := r.ClaimCommerceCustomerSyncJob(ctx, "controlled-commerce", "v1", time.Minute)
			if e != nil || current == nil || current.ID == old.ID || current.MappingVersion <= old.MappingVersion || current.ContactMobile != change.NewValue {
				t.Fatal("stale earliest candidate blocked current mobile", e)
			}
			if e = r.CompleteCommerceCustomerSyncJob(ctx, *current, commerce.CustomerProvisionResult{ExternalReferenceRedacted: "***1271"}); e != nil {
				t.Fatal(e)
			}
			if again, e := r.ClaimCommerceCustomerSyncJob(ctx, "controlled-commerce", "v1", time.Minute); e != nil || again != nil {
				t.Fatal("obsolete history dispatched again", e)
			}
			var count, attempts int
			if e = c.QueryRow(ctx, `SELECT count(*),COALESCE(max(attempts),0) FROM commerce_customer_sync_jobs WHERE id=$1`, old.ID).Scan(&count, &attempts); e != nil || count != 1 || attempts != old.Attempts {
				t.Fatal("old history mutated or lost", e)
			}
		})
	}
}
