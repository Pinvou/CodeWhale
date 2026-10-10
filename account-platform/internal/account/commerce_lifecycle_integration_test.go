package account

import (
	"context"
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
	t.Run("late_completion_records_stale_without_mapping_write", func(t *testing.T) {
		u, provider := seed(t)
		j := claim(t, provider)
		if _, e = c.Exec(ctx, `UPDATE users SET subject_lifecycle_version=3 WHERE id=$1`, u); e != nil {
			t.Fatal(e)
		}
		if e = r.CompleteCommerceCustomerSyncJob(ctx, *j, result); e != nil {
			t.Fatal(e)
		}
		var status, outcome string
		var version int64
		if e = c.QueryRow(ctx, `SELECT status,version FROM commerce_customer_mappings WHERE id=$1`, j.MappingID).Scan(&status, &version); e != nil || status != "IN_PROGRESS" || version != j.MappingVersion {
			t.Fatal("late completion revived mapping")
		}
		if e = c.QueryRow(ctx, `SELECT outcome FROM commerce_customer_sync_evidence WHERE job_id=$1`, j.ID).Scan(&outcome); e != nil || outcome != "STALE" {
			t.Fatal("late completion has no stale evidence")
		}
	})
	t.Run("late_retry_cannot_write_new_lifecycle_mapping", func(t *testing.T) {
		u, provider := seed(t)
		j := claim(t, provider)
		if _, e = c.Exec(ctx, `UPDATE users SET subject_lifecycle_version=3 WHERE id=$1`, u); e != nil {
			t.Fatal(e)
		}
		if e = r.RetryCommerceCustomerSyncJob(ctx, *j, "controlled failure", time.Now().UTC()); e != nil {
			t.Fatal(e)
		}
		var status string
		var version int64
		if e = c.QueryRow(ctx, `SELECT status,version FROM commerce_customer_mappings WHERE id=$1`, j.MappingID).Scan(&status, &version); e != nil || status != "IN_PROGRESS" || version != j.MappingVersion {
			t.Fatal("late retry changed mapping")
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
