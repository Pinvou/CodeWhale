package billingschema

import (
	"context"
	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/commerce"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCommerceClaimSubjectLockKeepsLeaseLive(t *testing.T) {
	if os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL") == "" {
		t.Skip("explicit isolated schema database required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL"))
	c := f.database(t, ctx, "account", "account_service")
	all, e := migrations.Load("../../migrations/account")
	if e != nil {
		t.Fatal(e)
	}
	if e = accountschema.Up(ctx, c, all); e != nil {
		t.Fatal(e)
	}
	migrations.Unlock(ctx, c)
	r, e := account.NewPostgresRepository(ctx, f.serviceURL(t, c, "account_service"))
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	if e = r.ConfigureCommerceProvider("closure-claim-probe", "v1"); e != nil {
		t.Fatal(e)
	}
	p := identity.Principal{Issuer: "controlled", Subject: uuid.NewString(), SessionID: uuid.NewString()}
	me, created, e := r.Bootstrap(ctx, p)
	if e != nil || !created {
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
	type outcome struct {
		job *account.CommerceCustomerSyncJob
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		j, e := r.ClaimCommerceCustomerSyncJob(ctx, "closure-claim-probe", "v1", 200*time.Millisecond)
		done <- outcome{j, e}
	}()
	var waiting bool
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, e = blocker.Exec(ctx, `SELECT pg_stat_clear_snapshot()`); e != nil {
			t.Fatal(e)
		}
		if e = blocker.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND $1=ANY(pg_blocking_pids(pid)) AND query LIKE '%SELECT status,subject_lifecycle_version FROM users%')`, blocker.PgConn().PID()).Scan(&waiting); e != nil {
			t.Fatal(e)
		}
		if waiting {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !waiting {
		t.Fatal("actual Claim did not wait on its selected user's row lock")
	}
	// The request is already inside the actual user lock wait. Let its old
	// sampled now+lease expire while the subject remains otherwise unchanged.
	time.Sleep(250 * time.Millisecond)
	if e = hold.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	out := <-done
	if out.err != nil || out.job == nil {
		t.Fatal(out.err)
	}
	var live bool
	if e = c.QueryRow(ctx, `SELECT lease_until>clock_timestamp() FROM commerce_customer_sync_jobs WHERE id=$1`, out.job.ID).Scan(&live); e != nil || !live {
		t.Fatalf("returned lease already expired: live=%v error=%v", live, e)
	}
	if e = r.CompleteCommerceCustomerSyncJob(ctx, *out.job, commerce.CustomerProvisionResult{ExternalReferenceRedacted: "***0001"}); e != nil {
		t.Fatal("live current claim could not complete", e)
	}
	var status string
	if e = c.QueryRow(ctx, `SELECT status FROM commerce_customer_mappings WHERE id=$1`, out.job.MappingID).Scan(&status); e != nil || status != "READY" {
		t.Fatal(status, e)
	}
	_ = strings.TrimSpace(status)
}

func TestCommerceClaimMappingLockKeepsLeaseLive(t *testing.T) {
	if os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL") == "" {
		t.Skip("explicit isolated schema database required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL"))
	c := f.database(t, ctx, "account", "account_service")
	all, e := migrations.Load("../../migrations/account")
	if e != nil {
		t.Fatal(e)
	}
	if e = accountschema.Up(ctx, c, all); e != nil {
		t.Fatal(e)
	}
	migrations.Unlock(ctx, c)
	r, e := account.NewPostgresRepository(ctx, f.serviceURL(t, c, "account_service"))
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	if e = r.ConfigureCommerceProvider("closure-claim-probe", "v1"); e != nil {
		t.Fatal(e)
	}
	p := identity.Principal{Issuer: "controlled", Subject: uuid.NewString(), SessionID: uuid.NewString()}
	me, created, e := r.Bootstrap(ctx, p)
	if e != nil || !created {
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
	if _, e = hold.Exec(ctx, `SELECT id FROM commerce_customer_mappings WHERE subject_type='USER' AND subject_id=$1 FOR UPDATE`, me.UserID); e != nil {
		t.Fatal(e)
	}
	type outcome struct {
		job *account.CommerceCustomerSyncJob
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		j, e := r.ClaimCommerceCustomerSyncJob(ctx, "closure-claim-probe", "v1", 200*time.Millisecond)
		done <- outcome{j, e}
	}()
	var waiting bool
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, e = blocker.Exec(ctx, `SELECT pg_stat_clear_snapshot()`); e != nil {
			t.Fatal(e)
		}
		if e = blocker.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND $1=ANY(pg_blocking_pids(pid)) AND query LIKE '%SELECT id,subject_type,subject_id,provider_id,adapter_version,version,status FROM commerce_customer_mappings%FOR UPDATE%')`, blocker.PgConn().PID()).Scan(&waiting); e != nil {
			t.Fatal(e)
		}
		if waiting {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !waiting {
		t.Fatal("actual Claim did not wait on its selected mapping row lock")
	}
	// The request is already inside the actual mapping lock wait. Let its old
	// sampled now+lease expire while the mapping remains otherwise unchanged.
	time.Sleep(250 * time.Millisecond)
	if e = hold.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	out := <-done
	if out.err != nil || out.job == nil {
		t.Fatal(out.err)
	}
	var live bool
	if e = c.QueryRow(ctx, `SELECT lease_until>clock_timestamp() FROM commerce_customer_sync_jobs WHERE id=$1`, out.job.ID).Scan(&live); e != nil || !live {
		t.Fatalf("returned lease already expired: live=%v error=%v", live, e)
	}
	if e = r.CompleteCommerceCustomerSyncJob(ctx, *out.job, commerce.CustomerProvisionResult{ExternalReferenceRedacted: "***0001"}); e != nil {
		t.Fatal("live current claim could not complete", e)
	}
	var status string
	if e = c.QueryRow(ctx, `SELECT status FROM commerce_customer_mappings WHERE id=$1`, out.job.MappingID).Scan(&status); e != nil || status != "READY" {
		t.Fatal(status, e)
	}
	_ = strings.TrimSpace(status)
}
