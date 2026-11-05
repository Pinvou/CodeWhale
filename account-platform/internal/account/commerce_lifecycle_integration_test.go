package account

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/commerce"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"os"
	"testing"
	"time"
)

// The existing owned coordinator guard creates only a UUID Account DB and fixed
// absent actors, applies public Account migrations, and performs normal cleanup.
func TestCommerceSyncLifecycleFence(t *testing.T) {
	raw := os.Getenv("F17_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("explicit owned PostgreSQL fixture required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Second)
	defer cancel()
	f := newAuditExportBoundaryFixture(t, ctx, raw)
	c := f.database(t, ctx, "account", "account_service")
	ms, e := migrations.Load("../../migrations/account")
	if e != nil {
		t.Fatal(e)
	}
	if e = accountschema.Up(ctx, c, ms); e != nil {
		t.Fatal(e)
	}
	if _, e = migrations.VerifyApplied(ctx, c, ms); e != nil {
		t.Fatal(e)
	}
	migrations.Unlock(ctx, c)
	r, e := NewPostgresRepository(ctx, f.serviceURL(t, c, "account_service"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(r.Close)
	seed := func(t *testing.T) (uuid.UUID, string) {
		t.Helper()
		p := identity.Principal{Issuer: "https://fixture.invalid", Subject: uuid.NewString(), SessionID: uuid.NewString(), AuthenticatedAt: time.Now().UTC()}
		me, _, e := r.Bootstrap(ctx, p)
		if e != nil {
			t.Fatal(e)
		}
		user := me.UserID
		provider := "fixture-" + uuid.NewString()
		if _, e = c.Exec(ctx, `UPDATE users SET status='ACTIVE',subject_lifecycle_version=2 WHERE id=$1`, user); e != nil {
			t.Fatal(e)
		}
		tx, e := r.pool.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		if e = enqueueCommerceCustomerSync(ctx, tx, "USER", user, "+8613800000000", provider, "v1", "CUSTOMER_PROVISIONING", time.Now().UTC()); e != nil {
			t.Fatal(e)
		}
		if e = tx.Commit(ctx); e != nil {
			t.Fatal(e)
		}
		return user, provider
	}
	claim := func(t *testing.T, provider string) *CommerceCustomerSyncJob {
		t.Helper()
		j, e := r.ClaimCommerceCustomerSyncJob(ctx, provider, "v1", time.Minute)
		if e != nil || j == nil {
			t.Fatalf("claim: %v", e)
		}
		return j
	}
	result := commerce.CustomerProvisionResult{ExternalReferenceRedacted: "customer***fixture"}
	snapshot := func(t *testing.T, user, mapping uuid.UUID) string {
		t.Helper()
		var frozen string
		if err := c.QueryRow(ctx, `SELECT jsonb_build_object(
'user',(SELECT to_jsonb(u) FROM users u WHERE id=$1),
'mapping',(SELECT to_jsonb(m) FROM commerce_customer_mappings m WHERE id=$2),
'jobs',(SELECT jsonb_agg(to_jsonb(j) ORDER BY j.id) FROM commerce_customer_sync_jobs j WHERE mapping_id=$2),
'evidence',(SELECT jsonb_agg(to_jsonb(e) ORDER BY e.id) FROM commerce_customer_sync_evidence e WHERE mapping_id=$2))::text`, user, mapping).Scan(&frozen); err != nil {
			t.Fatal(err)
		}
		return frozen
	}
	t.Run("current_generation_completes", func(t *testing.T) {
		_, provider := seed(t)
		j := claim(t, provider)
		if j.SubjectLifecycleVersion != 2 {
			t.Fatal("enqueue lost current lifecycle")
		}
		if e := r.CompleteCommerceCustomerSyncJob(ctx, *j, result); e != nil {
			t.Fatal(e)
		}
		var status string
		if e = c.QueryRow(ctx, `SELECT status FROM commerce_customer_mappings WHERE id=$1`, j.MappingID).Scan(&status); e != nil || status != "READY" {
			t.Fatal("current generation failed")
		}
	})
	t.Run("stale_pending_not_claimed", func(t *testing.T) {
		u, provider := seed(t)
		if _, e = c.Exec(ctx, `UPDATE users SET subject_lifecycle_version=3 WHERE id=$1`, u); e != nil {
			t.Fatal(e)
		}
		j, e := r.ClaimCommerceCustomerSyncJob(ctx, provider, "v1", time.Minute)
		if e != nil || j != nil {
			t.Fatal("stale pending generation reached supplier")
		}
	})
	t.Run("late_completion_rejects_old_lifecycle_without_effects", func(t *testing.T) {
		u, provider := seed(t)
		j := claim(t, provider)
		if _, e = c.Exec(ctx, `UPDATE users SET subject_lifecycle_version=3 WHERE id=$1`, u); e != nil {
			t.Fatal(e)
		}
		before := snapshot(t, u, j.MappingID)
		if err := r.CompleteCommerceCustomerSyncJob(ctx, *j, result); !errors.Is(err, ErrConflict) {
			t.Fatalf("old lifecycle completion must conflict: %v", err)
		}
		if snapshot(t, u, j.MappingID) != before {
			t.Fatal("old lifecycle completion changed user, mapping, jobs or evidence")
		}
	})
	t.Run("late_retry_cannot_write_new_lifecycle_mapping", func(t *testing.T) {
		u, provider := seed(t)
		j := claim(t, provider)
		if _, e = c.Exec(ctx, `UPDATE users SET subject_lifecycle_version=3 WHERE id=$1`, u); e != nil {
			t.Fatal(e)
		}
		before := snapshot(t, u, j.MappingID)
		if err := r.RetryCommerceCustomerSyncJob(ctx, *j, "controlled failure", time.Now().UTC()); !errors.Is(err, ErrConflict) {
			t.Fatalf("old lifecycle retry must conflict: %v", err)
		}
		if snapshot(t, u, j.MappingID) != before {
			t.Fatal("old lifecycle retry changed user, mapping, jobs or evidence")
		}
	})
	t.Run("mapping_version_drift_records_stale_in_same_lifecycle", func(t *testing.T) {
		u, provider := seed(t)
		j := claim(t, provider)
		if _, err := c.Exec(ctx, `UPDATE commerce_customer_mappings SET version=version+1 WHERE id=$1`, j.MappingID); err != nil {
			t.Fatal(err)
		}
		var mappingBefore, mappingAfter string
		if err := c.QueryRow(ctx, `SELECT to_jsonb(m)::text FROM commerce_customer_mappings m WHERE id=$1`, j.MappingID).Scan(&mappingBefore); err != nil {
			t.Fatal(err)
		}
		if err := r.CompleteCommerceCustomerSyncJob(ctx, *j, result); err != nil {
			t.Fatal(err)
		}
		if err := c.QueryRow(ctx, `SELECT to_jsonb(m)::text FROM commerce_customer_mappings m WHERE id=$1`, j.MappingID).Scan(&mappingAfter); err != nil || mappingAfter != mappingBefore {
			t.Fatal("stale mapping generation was changed", err)
		}
		var lifecycle int64
		var userStatus, jobStatus, outcome, reference string
		var leaseCleared, completed bool
		if err := c.QueryRow(ctx, `SELECT status,subject_lifecycle_version FROM users WHERE id=$1`, u).Scan(&userStatus, &lifecycle); err != nil || userStatus != "ACTIVE" || lifecycle != j.SubjectLifecycleVersion {
			t.Fatal("mapping drift fixture changed the USER lifecycle", err)
		}
		if err := c.QueryRow(ctx, `SELECT status,lease_until IS NULL,completed_at IS NOT NULL FROM commerce_customer_sync_jobs WHERE id=$1`, j.ID).Scan(&jobStatus, &leaseCleared, &completed); err != nil || jobStatus != "COMPLETED" || !leaseCleared || !completed {
			t.Fatal("same lifecycle stale callback did not finish its original lease", err)
		}
		if err := c.QueryRow(ctx, `SELECT outcome,external_reference_redacted FROM commerce_customer_sync_evidence WHERE job_id=$1`, j.ID).Scan(&outcome, &reference); err != nil || outcome != "STALE" || reference != result.ExternalReferenceRedacted {
			t.Fatal("same lifecycle mapping drift did not retain its stale evidence", err)
		}
	})
	t.Run("temporary_disable_preserves_same_generation_retry", func(t *testing.T) {
		u, provider := seed(t)
		j := claim(t, provider)
		if _, e = c.Exec(ctx, `UPDATE users SET status='DISABLED' WHERE id=$1`, u); e != nil {
			t.Fatal(e)
		}
		if e = r.CompleteCommerceCustomerSyncJob(ctx, *j, result); e != nil {
			t.Fatal(e)
		}
		var status string
		if e = c.QueryRow(ctx, `SELECT status FROM commerce_customer_sync_jobs WHERE id=$1`, j.ID).Scan(&status); e != nil || status != "RETRY" {
			t.Fatal("temporary disable retired generation")
		}
		if _, e = c.Exec(ctx, `UPDATE users SET status='ACTIVE' WHERE id=$1`, u); e != nil {
			t.Fatal(e)
		}
		j = claim(t, provider)
		if e = r.CompleteCommerceCustomerSyncJob(ctx, *j, result); e != nil {
			t.Fatal(e)
		}
		if e = c.QueryRow(ctx, `SELECT status FROM commerce_customer_mappings WHERE id=$1`, j.MappingID).Scan(&status); e != nil || status != "READY" {
			t.Fatal("enable could not recover same generation")
		}
	})
}
