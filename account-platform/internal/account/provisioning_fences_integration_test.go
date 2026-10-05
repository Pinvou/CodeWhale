package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/commerce"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

func provisioningUser(t *testing.T, repo *PostgresRepository) uuid.UUID {
	t.Helper()
	me, _, err := repo.Bootstrap(context.Background(), identity.Principal{Issuer: "https://identity.example.test", Subject: uuid.NewString(), SessionID: uuid.NewString(), Username: "pending" + uuid.NewString()[:8]})
	if err != nil {
		t.Fatal(err)
	}
	return me.UserID
}

func TestPostgresWalletProvisioningRejectsExpiredAndSupersededClaims(t *testing.T) {
	for _, scenario := range []string{"old_complete", "old_retry", "expired_complete", "expired_retry", "disabled", "current_complete", "current_retry"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			repo := accountDatabase(t)
			now := time.Now().UTC().Truncate(time.Microsecond)
			repo.now = func() time.Time { return now }
			userID := provisioningUser(t, repo)
			jobs, err := repo.LeaseWalletProvisioningJobs(ctx, 1, 30*time.Second)
			if err != nil || len(jobs) != 1 {
				t.Fatal("wallet job was not claimed")
			}
			old := jobs[0]
			if old.Attempts != 1 || old.LeaseUntil.IsZero() {
				t.Fatal("claim must expose its identity")
			}
			if scenario == "old_complete" || scenario == "old_retry" || scenario == "expired_complete" || scenario == "expired_retry" {
				now = now.Add(31 * time.Second)
			}
			var successor WalletProvisioningJob
			if scenario == "old_complete" || scenario == "old_retry" {
				jobs, err = repo.LeaseWalletProvisioningJobs(ctx, 1, 30*time.Second)
				if err != nil || len(jobs) != 1 {
					t.Fatal("expired claim was not reclaimed")
				}
				successor = jobs[0]
			}
			if scenario == "disabled" {
				if _, err := repo.pool.Exec(ctx, `UPDATE users SET status='DISABLED' WHERE id=$1`, userID); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "old_retry" || scenario == "expired_retry" || scenario == "current_retry" {
				err = repo.FailWalletProvisioning(ctx, old, "BILLING_UNAVAILABLE")
			} else {
				err = repo.CompleteWalletProvisioning(ctx, old)
			}
			valid := scenario == "current_complete" || scenario == "current_retry"
			if valid && err != nil || !valid && !errors.Is(err, ErrConflict) {
				t.Fatal("claim result did not respect the current lease")
			}
			var status string
			var attempts int
			var until *time.Time
			if err := repo.pool.QueryRow(ctx, `SELECT status,attempts,lease_until FROM wallet_provisioning_jobs WHERE user_id=$1`, userID).Scan(&status, &attempts, &until); err != nil {
				t.Fatal(err)
			}
			if !valid && (status != "LEASED" || until == nil) {
				t.Fatal("stale worker changed the current claim")
			}
			if successor.UserID != uuid.Nil && (attempts != successor.Attempts || !until.Equal(successor.LeaseUntil)) {
				t.Fatal("stale worker erased the successor lease")
			}
			if scenario == "current_complete" {
				if status != "COMPLETED" || until != nil {
					t.Fatal("current completion did not finish")
				}
				if !errors.Is(repo.CompleteWalletProvisioning(ctx, old), ErrConflict) {
					t.Fatal("completed claim was replayed")
				}
				var events int
				if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE event_type='account.wallet.provisioning.completed.v1' AND aggregate_id=$1`, userID).Scan(&events); err != nil || events != 1 {
					t.Fatal("completion event was not unique")
				}
			}
		})
	}
}

func TestPostgresCommerceProvisioningRejectsExpiredAndSupersededClaims(t *testing.T) {
	for _, scenario := range []string{"old_complete", "old_retry", "expired_complete", "expired_retry", "current_complete", "current_retry"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			repo := accountDatabase(t)
			now := time.Now().UTC().Truncate(time.Microsecond)
			repo.now = func() time.Time { return now }
			userID := provisioningUser(t, repo)
			if err := repo.ConfigureCommerceProvider("controlled", "v1"); err != nil {
				t.Fatal(err)
			}
			if _, err := repo.pool.Exec(ctx, `UPDATE users SET mobile_e164='+8613800138000',mobile_verified_at=$2 WHERE id=$1`, userID, now); err != nil {
				t.Fatal(err)
			}
			if err := repo.EnsureCommerceProviderMappings(ctx); err != nil {
				t.Fatal(err)
			}
			old, err := repo.ClaimCommerceCustomerSyncJob(ctx, "controlled", "v1", 30*time.Second)
			if err != nil || old == nil {
				t.Fatal("commerce job was not claimed")
			}
			if scenario != "current_complete" && scenario != "current_retry" {
				now = now.Add(31 * time.Second)
			}
			var successor *CommerceCustomerSyncJob
			if scenario == "old_complete" || scenario == "old_retry" {
				successor, err = repo.ClaimCommerceCustomerSyncJob(ctx, "controlled", "v1", 30*time.Second)
				if err != nil || successor == nil {
					t.Fatal("commerce claim was not reclaimed")
				}
			}
			if scenario == "old_retry" || scenario == "expired_retry" || scenario == "current_retry" {
				err = repo.RetryCommerceCustomerSyncJob(ctx, *old, "COMMERCE_PROVIDER_UNAVAILABLE", now.Add(time.Minute))
			} else {
				err = repo.CompleteCommerceCustomerSyncJob(ctx, *old, commerce.CustomerProvisionResult{ExternalReferenceRedacted: "***1234"})
			}
			valid := scenario == "current_complete" || scenario == "current_retry"
			if valid && err != nil || !valid && !errors.Is(err, ErrConflict) {
				t.Fatal("commerce write did not respect its lease")
			}
			var status string
			var attempts int
			var until *time.Time
			if err := repo.pool.QueryRow(ctx, `SELECT status,attempts,lease_until FROM commerce_customer_sync_jobs WHERE id=$1`, old.ID).Scan(&status, &attempts, &until); err != nil {
				t.Fatal(err)
			}
			if !valid && (status != "LEASED" || until == nil) {
				t.Fatal("stale commerce worker changed current state")
			}
			if successor != nil && (attempts != successor.Attempts || !until.Equal(successor.LeaseUntil)) {
				t.Fatal("stale worker erased successor lease")
			}
			var evidence int
			if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM commerce_customer_sync_evidence WHERE job_id=$1`, old.ID).Scan(&evidence); err != nil || (!valid && evidence != 0) || (valid && evidence != 1) {
				t.Fatal("claim evidence count is inconsistent")
			}
		})
	}
}
