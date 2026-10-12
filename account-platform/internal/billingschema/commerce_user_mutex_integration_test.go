package billingschema

import (
	"context"
	"fmt"
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
	"github.com/jackc/pgx/v5"
)

func TestF02CommerceClaimWaitStartsFreshLease(t *testing.T) {
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

	p := identity.Principal{Issuer: "https://owned.invalid", Subject: uuid.NewString(), SessionID: uuid.NewString()}
	u, _, err := r.Bootstrap(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	js, err := r.LeaseWalletProvisioningJobs(ctx, 20, time.Minute)
	if err != nil || len(js) != 1 {
		t.Fatal("wallet lease", err)
	}
	if err = r.CompleteWalletProvisioning(ctx, js[0]); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Exec(ctx, `UPDATE users SET mobile_e164='+8613800001250',mobile_verified_at=now() WHERE id=$1`, u.UserID); err != nil {
		t.Fatal(err)
	}
	if err = r.EnsureCommerceProviderMappings(ctx); err != nil {
		t.Fatal(err)
	}
	wait := func(check func() bool) {
		t.Helper()
		until := time.Now().Add(5 * time.Second)
		for !check() {
			if time.Now().After(until) {
				var states string
				_ = c.QueryRow(ctx, `SELECT COALESCE(string_agg(CASE WHEN query LIKE '%FROM commerce_customer_mappings%' THEN 'mapping' WHEN query LIKE '%FROM users%' THEN 'user' ELSE 'other' END || ':' || COALESCE(wait_event,'none'),','),'empty') FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND state='active'`).Scan(&states)
				t.Fatal("controlled wait not observed", states)
			}
			time.Sleep(time.Millisecond)
		}
	}

	var mappingID uuid.UUID
	if err = c.QueryRow(ctx, `SELECT id FROM commerce_customer_mappings WHERE subject_id=$1`, u.UserID).Scan(&mappingID); err != nil {
		t.Fatal(err)
	}
	for _, locked := range []string{"user", "mapping"} {
		t.Run(locked, func(t *testing.T) {
			gate, err := c.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer gate.Rollback(ctx)
			query := `SELECT id FROM commerce_customer_mappings WHERE id=$1 FOR UPDATE`
			id := mappingID
			if locked == "user" {
				query = `SELECT id FROM users WHERE id=$1 FOR UPDATE`
				id = u.UserID
			}
			if _, err = gate.Exec(ctx, query, id); err != nil {
				t.Fatal(err)
			}
			type response struct {
				job *account.CommerceCustomerSyncJob
				err error
			}
			done := make(chan response, 1)
			go func() {
				j, e := r.ClaimCommerceCustomerSyncJob(ctx, "controlled-commerce", "v1", 200*time.Millisecond)
				done <- response{j, e}
			}()
			wait(func() bool {
				var yes bool
				if err = gate.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND pg_backend_pid()=ANY(pg_blocking_pids(pid)))`).Scan(&yes); err != nil {
					t.Fatal(err)
				}
				return yes
			})
			time.Sleep(250 * time.Millisecond)
			if err = gate.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			result := <-done
			if result.err != nil || result.job == nil {
				t.Fatal("claim after real lock wait", result.err)
			}
			var live bool
			if err = c.QueryRow(ctx, `SELECT lease_until>clock_timestamp() FROM commerce_customer_sync_jobs WHERE id=$1`, result.job.ID).Scan(&live); err != nil {
				t.Fatal(err)
			}
			if !live {
				t.Fatal("claim returned expired lease after row wait")
			}
			wait(func() bool {
				var expired bool
				if err = c.QueryRow(ctx, `SELECT lease_until<clock_timestamp() FROM commerce_customer_sync_jobs WHERE id=$1`, result.job.ID).Scan(&expired); err != nil {
					t.Fatal(err)
				}
				return expired
			})
		})
	}
	// Both original public paths now wait in the same mapping-before-job order.
	old, err := r.ClaimCommerceCustomerSyncJob(ctx, "controlled-commerce", "v1", 200*time.Millisecond)
	if err != nil || old == nil {
		t.Fatal("old lease", err)
	}
	wait(func() bool {
		var expired bool
		if err = c.QueryRow(ctx, `SELECT lease_until<clock_timestamp() FROM commerce_customer_sync_jobs WHERE id=$1`, old.ID).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		return expired
	})
	gate, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(ctx)
	if _, err = gate.Exec(ctx, `SELECT id FROM commerce_customer_mappings WHERE id=$1 FOR UPDATE`, mappingID); err != nil {
		t.Fatal(err)
	}
	complete := make(chan error, 1)
	go func() {
		complete <- r.CompleteCommerceCustomerSyncJob(ctx, *old, commerce.CustomerProvisionResult{ExternalReferenceRedacted: "***late"})
	}()
	claimed := make(chan error, 1)
	go func() {
		j, e := r.ClaimCommerceCustomerSyncJob(ctx, "controlled-commerce", "v1", time.Minute)
		if e == nil && j == nil {
			e = account.ErrConflict
		}
		claimed <- e
	}()
	wait(func() bool {
		var n int
		if err = gate.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND cardinality(pg_blocking_pids(pid))>0`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 2
	})

	probe, err := pgx.Connect(ctx, f.serviceURL(t, c, "account_service"))
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close(context.Background())
	probeTx, err := probe.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer probeTx.Rollback(context.Background())
	if _, err = probeTx.Exec(ctx, `SELECT id FROM commerce_customer_sync_jobs WHERE id=$1 FOR UPDATE NOWAIT`, old.ID); err != nil {
		t.Fatal("writer retained job lock while waiting for earlier mutex", err)
	}
	if err = probeTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err = gate.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-complete; err != account.ErrConflict {
		t.Fatal("expired callback authority", err)
	}
	if err = <-claimed; err != nil {
		t.Fatal("claim/callback same-order progress", err)
	}
}

func TestF02CommerceConfirmedProducersPreserveStatuses(t *testing.T) {
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

	t.Run("real-registration-provisioning", func(t *testing.T) {
		op := account.RegistrationOperation{ID: uuid.New(), CandidateUserID: uuid.New(), QueryTokenHash: make([]byte, 32), RequestFingerprint: make([]byte, 32), Username: "registration" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12], Mobile: "+8613800001251", PasswordPolicyVersion: "controlled-v1"}
		challenge := account.SMSChallenge{ID: uuid.New(), Mobile: op.Mobile, Purpose: account.SMSPurposeRegister, ExpiresAt: time.Now().Add(5 * time.Minute), Source: uuid.NewString(), Device: uuid.NewString(), Provider: account.SMSProviderDescriptor{ID: "controlled-sms", AdapterVersion: "v1"}}
		if err = r.CreateRegistrationOperation(ctx, op, challenge); err != nil {
			t.Fatal(err)
		}
		// Persist the SMS/identity adapter ACK boundaries using their existing methods.
		if err = r.MarkSMSChallengeSent(ctx, challenge.ID, "controlled-receipt", nil); err != nil {
			t.Fatal(err)
		}
		if err = r.ConsumeRegistrationSMS(ctx, op.ID, challenge.ID, op.Mobile, true); err != nil {
			t.Fatal(err)
		}
		if err = r.MarkRegistrationIdentityCreating(ctx, op.ID); err != nil {
			t.Fatal(err)
		}
		if err = r.MarkRegistrationIdentityCreated(ctx, op.ID, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		receipt, e := r.CommitRegistration(ctx, op.ID, "https://owned.invalid")
		if e != nil || receipt.Status != "PROVISIONING" {
			t.Fatal("real registration enqueue", e)
		}
		var n int
		if e = c.QueryRow(ctx, `SELECT count(*) FROM commerce_customer_sync_jobs WHERE subject_id=$1 AND status='PENDING' AND task_category='CUSTOMER_PROVISIONING'`, op.CandidateUserID).Scan(&n); e != nil || n != 1 {
			t.Fatal("provisioning lost job", e)
		}
		if j, e := r.ClaimCommerceCustomerSyncJob(ctx, "controlled-commerce", "v1", time.Minute); e != nil || j != nil {
			t.Fatal("provisioning dispatched early", e)
		}
		jobs, e := r.LeaseWalletProvisioningJobs(ctx, 20, time.Minute)
		if e != nil || len(jobs) != 1 {
			t.Fatal("registration wallet job", e)
		}
		if e = r.CompleteWalletProvisioning(ctx, jobs[0]); e != nil {
			t.Fatal(e)
		}
		j, e := r.ClaimCommerceCustomerSyncJob(ctx, "controlled-commerce", "v1", time.Minute)
		if e != nil || j == nil {
			t.Fatal("active registration dispatch", e)
		}
		if e = r.CompleteCommerceCustomerSyncJob(ctx, *j, commerce.CustomerProvisionResult{ExternalReferenceRedacted: "***registration"}); e != nil {
			t.Fatal(e)
		}
	})
	for index, status := range []string{"ACTIVE", "DISABLED", "REPAIR_REQUIRED", "PROVISIONING"} {
		t.Run("confirmed-mobile-"+status, func(t *testing.T) {
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
			change, e := r.CreateProfileChange(ctx, p, "MOBILE", fmt.Sprintf("+861380000126%d", index))
			if e != nil {
				t.Fatal(e)
			}
			if e = r.MarkProfileIdentityUpdated(ctx, change.ID); e != nil {
				t.Fatal(e)
			}
			if _, e = c.Exec(ctx, `UPDATE users SET status=$2 WHERE id=$1`, u.UserID, status); e != nil {
				t.Fatal(e)
			}
			if e = r.CommitProfileChange(ctx, change.ID); e != nil {
				t.Fatal("confirmed mobile must preserve old commit contract", e)
			}
			if e = r.CommitProfileChange(ctx, change.ID); e != nil {
				t.Fatal("confirmed mobile replay", e)
			}
			var n int
			if e = c.QueryRow(ctx, `SELECT count(*) FROM commerce_customer_sync_jobs WHERE subject_id=$1 AND status='PENDING' AND task_category='PROFILE_SYNC'`, u.UserID).Scan(&n); e != nil || n != 1 {
				t.Fatal("confirmed mobile enqueue exactly once", e)
			}
			if status != "ACTIVE" {
				if j, e := r.ClaimCommerceCustomerSyncJob(ctx, "controlled-commerce", "v1", time.Minute); e != nil || j != nil {
					t.Fatal("inactive confirmed mobile dispatched", e)
				}
				if _, e = c.Exec(ctx, `UPDATE users SET status='ACTIVE' WHERE id=$1`, u.UserID); e != nil {
					t.Fatal(e)
				}
			}
			j, e := r.ClaimCommerceCustomerSyncJob(ctx, "controlled-commerce", "v1", time.Minute)
			if e != nil || j == nil || j.ContactMobile != change.NewValue {
				t.Fatal("active confirmed mobile dispatch", e)
			}
			if e = r.CompleteCommerceCustomerSyncJob(ctx, *j, commerce.CustomerProvisionResult{ExternalReferenceRedacted: "***mobile"}); e != nil {
				t.Fatal(e)
			}
		})
	}
}
