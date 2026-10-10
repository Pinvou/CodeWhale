package billingschema

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/commerce"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
)

// Install only the public product migrations, under the real migrator/service
// actors. No original-only migration directory or fixture DDL supplies tables.
func TestF02PublicCommerceCustomerLifecycle(t *testing.T) {
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
		t.Fatal("public Account migrations", err)
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
	principal := identity.Principal{Issuer: "https://owned.invalid", Subject: uuid.NewString(), SessionID: uuid.NewString()}
	var me account.Me
	t.Run("bootstrap-wallet-ack-and-get-me", func(t *testing.T) {
		var created bool
		me, created, err = r.Bootstrap(ctx, principal)
		if err != nil || !created || me.Status != "PROVISIONING" || me.UserID == uuid.Nil {
			t.Fatalf("public-schema Bootstrap: created=%v status=%s err=%v", created, me.Status, err)
		}
		replay, created, e := r.Bootstrap(ctx, principal)
		if e != nil || created || replay.UserID != me.UserID {
			t.Fatal("Bootstrap replay changed identity", e)
		}
		jobs, e := r.LeaseWalletProvisioningJobs(ctx, 20, time.Minute)
		if e != nil || len(jobs) != 1 || jobs[0].UserID != me.UserID {
			t.Fatal("actual wallet job not leased", e)
		}
		// This is the Account-side receipt of the wallet provisioner, not a
		// claim that a Billing HTTP request or provider was executed here.
		if e = r.CompleteWalletProvisioning(ctx, jobs[0]); e != nil {
			t.Fatal(e)
		}
		me, e = r.GetMe(ctx, principal)
		if e != nil || me.Status != "ACTIVE" || me.CommerceCustomerStatus != "NOT_CONFIGURED" {
			t.Fatal("public-schema GetMe after wallet ACK", e)
		}
	})
	if me.UserID == uuid.Nil || me.Status != "ACTIVE" {
		return // Preserve the actual Bootstrap failure; do not manufacture state.
	}
	t.Run("current-schema-without-future-closure", func(t *testing.T) {
		var present, future bool
		if e := c.QueryRow(ctx, `SELECT to_regclass('commerce_customer_mappings') IS NOT NULL AND to_regclass('commerce_customer_sync_jobs') IS NOT NULL AND to_regclass('commerce_customer_sync_evidence') IS NOT NULL AND to_regclass('mall_user_sync_jobs') IS NULL, EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='users' AND column_name='subject_lifecycle_version')`).Scan(&present, &future); e != nil || !present || future {
			t.Fatal("current Commerce tables or future closure boundary incorrect", e)
		}
		if _, e := migrations.VerifyApplied(ctx, c, all); e != nil {
			t.Fatal(e)
		}
	})
	if _, err = c.Exec(ctx, `UPDATE users SET mobile_e164='+8613800001234',mobile_verified_at=now() WHERE id=$1`, me.UserID); err != nil {
		t.Fatal(err)
	}
	t.Run("startup-ensure-claim-retry-complete", func(t *testing.T) {
		if e := r.EnsureCommerceProviderMappings(ctx); e != nil {
			t.Fatal("startup EnsureMappings", e)
		}
		job, e := r.ClaimCommerceCustomerSyncJob(ctx, "controlled-commerce", "v1", time.Minute)
		if e != nil || job == nil || job.SubjectID != me.UserID || job.SubjectLifecycleVersion != 1 || job.Attempts != 1 {
			t.Fatal("actual public-schema claim", e)
		}
		if e = r.RetryCommerceCustomerSyncJob(ctx, *job, "COMMERCE_PROVIDER_UNAVAILABLE", time.Now().Add(-time.Second)); e != nil {
			t.Fatal("actual public-schema retry", e)
		}
		mapping, e := r.GetCommerceCustomerMapping(ctx, principal)
		if e != nil || mapping.Status != "RETRY" {
			t.Fatal("retry projection", e)
		}
		job, e = r.ClaimCommerceCustomerSyncJob(ctx, "controlled-commerce", "v1", time.Minute)
		if e != nil || job == nil || job.Attempts != 2 {
			t.Fatal("retry reclaim", e)
		}
		if e = r.CompleteCommerceCustomerSyncJob(ctx, *job, commerce.CustomerProvisionResult{ExternalReferenceRedacted: "***1234"}); e != nil {
			t.Fatal("actual public-schema completion", e)
		}
		current, e := r.GetMe(ctx, principal)
		if e != nil || current.CommerceCustomerStatus != "READY" {
			t.Fatal("READY read projection", e)
		}
		var failed, succeeded int
		if e = c.QueryRow(ctx, `SELECT count(*) FILTER(WHERE outcome='FAILED'),count(*) FILTER(WHERE outcome='SUCCEEDED') FROM commerce_customer_sync_evidence WHERE mapping_id=$1`, job.MappingID).Scan(&failed, &succeeded); e != nil || failed != 1 || succeeded != 1 {
			t.Fatal("actual retry/success evidence lost", e)
		}
	})
	if !t.Run("disabled-user-cannot-claim-or-complete", func(t *testing.T) {
		if _, e := c.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,source,starts_at,created_at) VALUES($1,$2,'PLATFORM_IDENTITY_ADMIN','ACCOUNT',now(),now())`, uuid.New(), me.UserID); e != nil {
			t.Fatal(e)
		}
		decision, e := r.Authorize(ctx, principal, "platform.users.status.manage", nil, nil)
		if e != nil || !decision.Allowed {
			t.Fatal("real status actor authority", e)
		}
		p := identity.Principal{Issuer: principal.Issuer, Subject: uuid.NewString(), SessionID: uuid.NewString()}
		u, _, e := r.Bootstrap(ctx, p)
		if e != nil {
			t.Fatal(e)
		}
		walletJobs, e := r.LeaseWalletProvisioningJobs(ctx, 20, time.Minute)
		if e != nil || len(walletJobs) != 1 || walletJobs[0].UserID != u.UserID {
			t.Fatal("actual wallet job not leased for current user", e)
		}
		if e = r.CompleteWalletProvisioning(ctx, walletJobs[0]); e != nil {
			t.Fatal(e)
		}
		if _, e = c.Exec(ctx, `UPDATE users SET mobile_e164='+8613800001235',mobile_verified_at=now() WHERE id=$1`, u.UserID); e != nil {
			t.Fatal(e)
		}
		if e = r.EnsureCommerceProviderMappings(ctx); e != nil {
			t.Fatal(e)
		}
		if e = r.AdminSetUserStatus(ctx, principal, u.UserID, "DISABLED"); e != nil {
			t.Fatal(e)
		}
		if job, e := r.ClaimCommerceCustomerSyncJob(ctx, "controlled-commerce", "v1", time.Minute); e != nil || job != nil {
			t.Fatal("disabled user claimed", e)
		}
		if e = r.AdminSetUserStatus(ctx, principal, u.UserID, "ACTIVE"); e != nil {
			t.Fatal(e)
		}
		job, e := r.ClaimCommerceCustomerSyncJob(ctx, "controlled-commerce", "v1", time.Minute)
		if e != nil || job == nil || job.SubjectID != u.UserID {
			t.Fatal("active user claim", e)
		}
		if e = r.AdminSetUserStatus(ctx, principal, u.UserID, "DISABLED"); e != nil {
			t.Fatal(e)
		}
		if e = r.RetryCommerceCustomerSyncJob(ctx, *job, "COMMERCE_PROVIDER_UNAVAILABLE", time.Now().Add(-time.Second)); e != nil {
			t.Fatal(e)
		}
		mapping, e := r.GetCommerceCustomerMapping(ctx, p)
		if e != nil || mapping.Status != "IN_PROGRESS" {
			t.Fatal("disabled late retry changed mapping", e)
		}
		// Re-enable through the supported status fact and reclaim the same
		// retry. Disable again while the provider is in flight.
		if e = r.AdminSetUserStatus(ctx, principal, u.UserID, "ACTIVE"); e != nil {
			t.Fatal(e)
		}
		job, e = r.ClaimCommerceCustomerSyncJob(ctx, "controlled-commerce", "v1", time.Minute)
		if e != nil || job == nil {
			t.Fatal("active retry not reclaimed", e)
		}
		if e = r.AdminSetUserStatus(ctx, principal, u.UserID, "DISABLED"); e != nil {
			t.Fatal(e)
		}
		if e = r.CompleteCommerceCustomerSyncJob(ctx, *job, commerce.CustomerProvisionResult{ExternalReferenceRedacted: "***1235"}); e != nil {
			t.Fatal(e)
		}
		mapping, e = r.GetCommerceCustomerMapping(ctx, p)
		if e != nil || mapping.Status == "READY" {
			t.Fatal("disabled late result became READY", e)
		}
		var outcome string
		if e = c.QueryRow(ctx, `SELECT outcome FROM commerce_customer_sync_evidence WHERE job_id=$1 AND outcome='STALE'`, job.ID).Scan(&outcome); e != nil || outcome != "STALE" {
			t.Fatal("disabled late result evidence", e)
		}
		if denied, e := r.ClaimCommerceCustomerSyncJob(ctx, "controlled-commerce", "v1", time.Minute); e != nil || denied != nil {
			t.Fatal("disabled recoverable job was claimable", e)
		}
		if e = r.AdminSetUserStatus(ctx, principal, u.UserID, "ACTIVE"); e != nil {
			t.Fatal(e)
		}
		if e = r.EnsureCommerceProviderMappings(ctx); e != nil {
			t.Fatal(e)
		}
		recovered, e := r.ClaimCommerceCustomerSyncJob(ctx, "controlled-commerce", "v1", time.Minute)
		if e != nil || recovered == nil || recovered.SubjectID != u.UserID || recovered.MappingVersion != job.MappingVersion {
			t.Fatal("enabled user lost recoverable same-generation sync", e)
		}
		if e = r.CompleteCommerceCustomerSyncJob(ctx, *recovered, commerce.CustomerProvisionResult{ExternalReferenceRedacted: "***1235"}); e != nil {
			t.Fatal(e)
		}
		mapping, e = r.GetCommerceCustomerMapping(ctx, p)
		if e != nil || mapping.Status != "READY" {
			t.Fatal("enabled user sync failed to recover", e)
		}
	}) {
		return
	}
	t.Run("mapping-generation-and-adapter-fence", func(t *testing.T) {
		p := identity.Principal{Issuer: principal.Issuer, Subject: uuid.NewString(), SessionID: uuid.NewString()}
		u, _, e := r.Bootstrap(ctx, p)
		if e != nil {
			t.Fatal(e)
		}
		walletJobs, e := r.LeaseWalletProvisioningJobs(ctx, 20, time.Minute)
		if e != nil || len(walletJobs) != 1 || walletJobs[0].UserID != u.UserID {
			t.Fatal("actual wallet job not leased for current user", e)
		}
		if e = r.CompleteWalletProvisioning(ctx, walletJobs[0]); e != nil {
			t.Fatal(e)
		}
		if _, e = c.Exec(ctx, `UPDATE users SET mobile_e164='+8613800001236',mobile_verified_at=now() WHERE id=$1`, u.UserID); e != nil {
			t.Fatal(e)
		}
		if e = r.EnsureCommerceProviderMappings(ctx); e != nil {
			t.Fatal(e)
		}
		old, e := r.ClaimCommerceCustomerSyncJob(ctx, "controlled-commerce", "v1", time.Minute)
		if e != nil || old == nil || old.SubjectID != u.UserID {
			t.Fatal("initial adapter job", e)
		}
		if e = r.ConfigureCommerceProvider("controlled-commerce", "v2"); e != nil {
			t.Fatal(e)
		}
		if e = r.EnsureCommerceProviderMappings(ctx); e != nil {
			t.Fatal(e)
		}
		before, e := r.GetCommerceCustomerMapping(ctx, p)
		if e != nil || before.Status != "PENDING" || before.AdapterVersion != "v2" {
			t.Fatal("new adapter generation", e)
		}
		if e = r.RetryCommerceCustomerSyncJob(ctx, *old, "COMMERCE_PROVIDER_UNAVAILABLE", time.Now().Add(-time.Second)); e != nil {
			t.Fatal(e)
		}
		stale, e := r.ClaimCommerceCustomerSyncJob(ctx, "controlled-commerce", "v1", time.Minute)
		if e != nil || stale != nil {
			t.Fatal("old generation acquired a new dispatch lease", e)
		}
		after, e := r.GetCommerceCustomerMapping(ctx, p)
		if e != nil || before != after {
			t.Fatal("stale retry/completion changed current adapter mapping", e)
		}
		var outcome string
		if e = c.QueryRow(ctx, `SELECT outcome FROM commerce_customer_sync_evidence WHERE job_id=$1 AND outcome='FAILED'`, old.ID).Scan(&outcome); e != nil || outcome != "FAILED" {
			t.Fatal("old generation retry evidence retained", e)
		}
		current, e := r.ClaimCommerceCustomerSyncJob(ctx, "controlled-commerce", "v2", time.Minute)
		if e != nil || current == nil || current.MappingVersion <= old.MappingVersion {
			t.Fatal("new generation not claimable", e)
		}
		if e = r.CompleteCommerceCustomerSyncJob(ctx, *current, commerce.CustomerProvisionResult{ExternalReferenceRedacted: "***1236"}); e != nil {
			t.Fatal(e)
		}
		after, e = r.GetCommerceCustomerMapping(ctx, p)
		if e != nil || after.Status != "READY" || after.AdapterVersion != "v2" {
			t.Fatal("new generation completion", e)
		}
	})
	t.Run("detached-current-mapping-cannot-resume", func(t *testing.T) {
		p := identity.Principal{Issuer: principal.Issuer, Subject: uuid.NewString(), SessionID: uuid.NewString()}
		u, _, e := r.Bootstrap(ctx, p)
		if e != nil {
			t.Fatal(e)
		}
		walletJobs, e := r.LeaseWalletProvisioningJobs(ctx, 20, time.Minute)
		if e != nil || len(walletJobs) != 1 || walletJobs[0].UserID != u.UserID {
			t.Fatal("actual wallet job not leased for current user", e)
		}
		if e = r.CompleteWalletProvisioning(ctx, walletJobs[0]); e != nil {
			t.Fatal(e)
		}
		if _, e = c.Exec(ctx, `UPDATE users SET mobile_e164='+8613800001237',mobile_verified_at=now() WHERE id=$1`, u.UserID); e != nil {
			t.Fatal(e)
		}
		if e = r.EnsureCommerceProviderMappings(ctx); e != nil {
			t.Fatal(e)
		}
		job, e := r.ClaimCommerceCustomerSyncJob(ctx, "controlled-commerce", "v2", time.Minute)
		if e != nil || job == nil || job.SubjectID != u.UserID {
			t.Fatal("new detached candidate job", e)
		}
		// This stage has no detach API: model the durable pre-existing marker,
		// without claiming an uninstalled closure producer ran here.
		if _, e = c.Exec(ctx, `UPDATE commerce_customer_mappings SET status='DETACHED' WHERE id=$1`, job.MappingID); e != nil {
			t.Fatal(e)
		}
		if e = r.CompleteCommerceCustomerSyncJob(ctx, *job, commerce.CustomerProvisionResult{ExternalReferenceRedacted: "***obsolete"}); e != nil {
			t.Fatal(e)
		}
		mapping, e := r.GetCommerceCustomerMapping(ctx, p)
		if e != nil || mapping.Status != "DETACHED" {
			t.Fatal("detached mapping accepted late result", e)
		}
		var status, outcome string
		if e = c.QueryRow(ctx, `SELECT j.status,e.outcome FROM commerce_customer_sync_jobs j JOIN commerce_customer_sync_evidence e ON e.job_id=j.id WHERE j.id=$1`, job.ID).Scan(&status, &outcome); e != nil || status != "COMPLETED" || outcome != "STALE" {
			t.Fatal("detached job was made recoverable", e)
		}
		if repeated, e := r.ClaimCommerceCustomerSyncJob(ctx, "controlled-commerce", "v2", time.Minute); e != nil || repeated != nil {
			t.Fatal("detached late job became claimable", e)
		}
	})
	t.Run("reclaimed-lease-rejects-previous-worker", func(t *testing.T) {
		for _, action := range []string{"complete", "retry"} {
			t.Run(action, func(t *testing.T) {
				p := identity.Principal{Issuer: principal.Issuer, Subject: uuid.NewString(), SessionID: uuid.NewString()}
				u, _, e := r.Bootstrap(ctx, p)
				if e != nil {
					t.Fatal(e)
				}
				walletJobs, e := r.LeaseWalletProvisioningJobs(ctx, 20, time.Minute)
				if e != nil || len(walletJobs) != 1 || walletJobs[0].UserID != u.UserID {
					t.Fatal("actual wallet job not leased for current user", e)
				}
				if e = r.CompleteWalletProvisioning(ctx, walletJobs[0]); e != nil {
					t.Fatal(e)
				}
				mobile := "+8613800001238"
				if action == "retry" {
					mobile = "+8613800001239"
				}
				if _, e = c.Exec(ctx, `UPDATE users SET mobile_e164=$2,mobile_verified_at=now() WHERE id=$1`, u.UserID, mobile); e != nil {
					t.Fatal(e)
				}
				if e = r.EnsureCommerceProviderMappings(ctx); e != nil {
					t.Fatal(e)
				}
				first, e := r.ClaimCommerceCustomerSyncJob(ctx, "controlled-commerce", "v2", 20*time.Millisecond)
				if e != nil || first == nil || first.SubjectID != u.UserID {
					t.Fatal("first legitimate lease", e)
				}
				deadline := time.Now().Add(time.Second)
				for {
					var expired bool
					var leaseUntil time.Time
					if e = c.QueryRow(ctx, `SELECT clock_timestamp()>lease_until,lease_until FROM commerce_customer_sync_jobs WHERE id=$1`, first.ID).Scan(&expired, &leaseUntil); e != nil {
						t.Fatal(e)
					}
					// Claim uses the repository's real Go clock. PostgreSQL's
					// clock alone can cross this 20ms boundary first on Windows.
					// Require expiry in both clock domains without changing the
					// lease or weakening the subsequent reclaim assertion.
					applicationExpired := time.Now().After(leaseUntil)
					if expired && !applicationExpired {
						t.Log("database lease clock passed expiry before the application claim clock")
					}
					if expired && applicationExpired {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("real lease did not expire")
					}
					time.Sleep(time.Millisecond)
				}
				second, e := r.ClaimCommerceCustomerSyncJob(ctx, "controlled-commerce", "v2", time.Minute)
				if e != nil || second == nil || second.ID != first.ID || second.Attempts != first.Attempts+1 {
					t.Fatal("expired lease was not actually reclaimed", e)
				}
				if action == "complete" {
					e = r.CompleteCommerceCustomerSyncJob(ctx, *first, commerce.CustomerProvisionResult{ExternalReferenceRedacted: "***old-worker"})
				} else {
					e = r.RetryCommerceCustomerSyncJob(ctx, *first, "COMMERCE_PROVIDER_UNAVAILABLE", time.Now().Add(time.Hour))
				}
				if e != nil && e != account.ErrConflict {
					t.Fatal("unexpected old worker error", e)
				}
				var status, mappingStatus string
				var attempts, evidence int
				if q := c.QueryRow(ctx, `SELECT j.status,j.attempts,m.status,(SELECT count(*) FROM commerce_customer_sync_evidence e WHERE e.job_id=j.id) FROM commerce_customer_sync_jobs j JOIN commerce_customer_mappings m ON m.id=j.mapping_id WHERE j.id=$1`, second.ID).Scan(&status, &attempts, &mappingStatus, &evidence); q != nil || status != "LEASED" || attempts != second.Attempts || mappingStatus != "IN_PROGRESS" || evidence != 0 {
					t.Fatalf("old %s modified current lease: job=%s attempts=%d mapping=%s evidence=%d err=%v", action, status, attempts, mappingStatus, evidence, q)
				}
				if e != account.ErrConflict {
					t.Fatal("old worker authority was accepted")
				}
				if e = r.CompleteCommerceCustomerSyncJob(ctx, *second, commerce.CustomerProvisionResult{ExternalReferenceRedacted: "***current-worker"}); e != nil {
					t.Fatal("current lease could not complete", e)
				}
			})
		}
	})
}

func TestF02CommerceLeaseExpiresWhileWaitingForMappingLock(t *testing.T) {
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
	for index, action := range []string{"complete", "retry"} {
		t.Run(action, func(t *testing.T) {
			p := identity.Principal{Issuer: "https://owned.invalid", Subject: uuid.NewString(), SessionID: uuid.NewString()}
			u, _, e := r.Bootstrap(ctx, p)
			if e != nil {
				t.Fatal(e)
			}
			walletJobs, e := r.LeaseWalletProvisioningJobs(ctx, 20, time.Minute)
			if e != nil || len(walletJobs) != 1 || walletJobs[0].UserID != u.UserID {
				t.Fatal("actual wallet job not leased for current user", e)
			}
			if e = r.CompleteWalletProvisioning(ctx, walletJobs[0]); e != nil {
				t.Fatal(e)
			}
			mobile := []string{"+8613800001240", "+8613800001241"}[index]
			if _, e = c.Exec(ctx, `UPDATE users SET mobile_e164=$2,mobile_verified_at=now() WHERE id=$1`, u.UserID, mobile); e != nil {
				t.Fatal(e)
			}
			if e = r.EnsureCommerceProviderMappings(ctx); e != nil {
				t.Fatal(e)
			}
			job, e := r.ClaimCommerceCustomerSyncJob(ctx, "controlled-commerce", "v1", 300*time.Millisecond)
			if e != nil || job == nil || job.SubjectID != u.UserID {
				t.Fatal("first legitimate lease", e)
			}
			blocker, e := c.Begin(ctx)
			if e != nil {
				t.Fatal(e)
			}
			defer blocker.Rollback(ctx)
			if _, e = blocker.Exec(ctx, `SELECT id FROM commerce_customer_mappings WHERE id=$1 FOR UPDATE`, job.MappingID); e != nil {
				t.Fatal(e)
			}
			result := make(chan error, 1)
			go func() {
				if action == "complete" {
					result <- r.CompleteCommerceCustomerSyncJob(ctx, *job, commerce.CustomerProvisionResult{ExternalReferenceRedacted: "***late"})
				} else {
					result <- r.RetryCommerceCustomerSyncJob(ctx, *job, "COMMERCE_PROVIDER_UNAVAILABLE", time.Now().Add(time.Hour))
				}
			}()
			deadline := time.Now().Add(2 * time.Second)
			for {
				var waiting bool
				if e = blocker.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid WHERE a.datname=current_database() AND l.pid<>pg_backend_pid() AND NOT l.granted)`).Scan(&waiting); e != nil {
					t.Fatal(e)
				}
				if waiting {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("worker did not actually wait for mapping lock")
				}
				time.Sleep(time.Millisecond)
			}
			for {
				var expired bool
				if e = blocker.QueryRow(ctx, `SELECT clock_timestamp()>lease_until FROM commerce_customer_sync_jobs WHERE id=$1`, job.ID).Scan(&expired); e != nil {
					t.Fatal(e)
				}
				if expired {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("database lease did not expire during row-lock wait")
				}
				time.Sleep(time.Millisecond)
			}
			if e = blocker.Rollback(ctx); e != nil {
				t.Fatal(e)
			}
			select {
			case e = <-result:
				if e != account.ErrConflict {
					t.Fatal("expired waiter retained mutation authority", e)
				}
			case <-ctx.Done():
				t.Fatal("worker did not finish after row lock release")
			}
			var status, mappingStatus string
			var evidence int
			if e = c.QueryRow(ctx, `SELECT j.status,m.status,(SELECT count(*) FROM commerce_customer_sync_evidence e WHERE e.job_id=j.id) FROM commerce_customer_sync_jobs j JOIN commerce_customer_mappings m ON m.id=j.mapping_id WHERE j.id=$1`, job.ID).Scan(&status, &mappingStatus, &evidence); e != nil || status != "LEASED" || mappingStatus != "IN_PROGRESS" || evidence != 0 {
				t.Fatal("expired lock waiter wrote persistent result", e)
			}
			current, e := r.ClaimCommerceCustomerSyncJob(ctx, "controlled-commerce", "v1", time.Minute)
			if e != nil || current == nil || current.ID != job.ID || current.Attempts != job.Attempts+1 {
				t.Fatal("rejected waiter lost recoverable job", e)
			}
			if e = r.CompleteCommerceCustomerSyncJob(ctx, *current, commerce.CustomerProvisionResult{ExternalReferenceRedacted: "***current"}); e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestF02CommerceOriginal15LateUpgradePreservesFacts(t *testing.T) {
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
	var prior []migrations.Migration
	for _, m := range all {
		if m.Version != 15 && m.Version != 91 {
			prior = append(prior, m)
		}
	}
	if err = accountschema.Up(ctx, c, prior); err != nil {
		t.Fatal(err)
	}
	// Do not bypass the mature outbox INSERT function ACL. This upgrade fixture
	// seeds identity facts only; the lifecycle test above writes real outbox
	// events through Bootstrap and the wallet state machine.
	if _, err = c.Exec(ctx, strings.Split(stageAccountSeed, "INSERT INTO outbox_events")[0]); err != nil {
		t.Fatal(err)
	}
	before := stageAccountFacts(t, ctx, c)
	if _, err = c.Exec(ctx, `INSERT INTO mall_user_sync_jobs(user_id,mobile_e164,status,buyer_id,attempts,next_attempt_at,created_at,updated_at,synced_at) VALUES('11111111-1111-4111-8111-111111111111','+8613800001234','SYNCED','controlled-1234',0,now(),now(),now(),now())`); err != nil {
		t.Fatal(err)
	}
	if err = accountschema.Up(ctx, c, all); err != nil {
		t.Fatal("late original 015", err)
	}
	if before != stageAccountFacts(t, ctx, c) {
		t.Fatal("late 015 changed identity/session/authority/outbox facts")
	}
	var status, reference string
	if err = c.QueryRow(ctx, `SELECT status,external_reference_redacted FROM commerce_customer_mappings WHERE subject_id='11111111-1111-4111-8111-111111111111' AND provider_id='h3c_mall'`).Scan(&status, &reference); err != nil || status != "READY" || reference != "***1234" {
		t.Fatal("original migration did not preserve/redact legacy customer facts", err)
	}
	if _, err = migrations.VerifyApplied(ctx, c, all); err != nil {
		t.Fatal(err)
	}
	var indexDefinition string
	if err = c.QueryRow(ctx, `SELECT pg_get_indexdef('commerce_customer_sync_jobs_leased_idx'::regclass)`).Scan(&indexDefinition); err != nil || !strings.Contains(indexDefinition, "lease_until") || !strings.Contains(indexDefinition, "LEASED") {
		t.Fatal("late lease index not installed", err)
	}
	if err = accountschema.Up(ctx, c, all); err != nil {
		t.Fatal("late upgrade replay", err)
	}
	if _, err = migrations.VerifyApplied(ctx, c, all); err != nil {
		t.Fatal("late replay checksum", err)
	}
	var oldTable *string
	if err = c.QueryRow(ctx, `SELECT to_regclass('mall_user_sync_jobs')::text`).Scan(&oldTable); err != nil || oldTable != nil {
		t.Fatal("old table still present", err)
	}
}
