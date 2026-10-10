package account

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type WalletProvisioningJob struct {
	UserID                  uuid.UUID
	RegistrationOperationID *uuid.UUID
	PolicyVersion           string
	SLAExpiresAt            time.Time
	Attempts                int
	LeaseUntil              time.Time
	RegistrationBonusMode   string
	FingerprintKeyID        string
	MobileFingerprint       []byte
}

type WalletProvisioningRepository interface {
	LeaseWalletProvisioningJobs(context.Context, int, time.Duration) ([]WalletProvisioningJob, error)
	CompleteWalletProvisioning(context.Context, WalletProvisioningJob) error
	FailWalletProvisioning(context.Context, WalletProvisioningJob, string) error
}

type WalletProvisioningWorker struct {
	repository WalletProvisioningRepository
	wallets    WalletProvisioner
	interval   time.Duration
}

func NewWalletProvisioningWorker(repository WalletProvisioningRepository, wallets WalletProvisioner, interval time.Duration) *WalletProvisioningWorker {
	return &WalletProvisioningWorker{repository: repository, wallets: wallets, interval: interval}
}

func (w *WalletProvisioningWorker) Run(ctx context.Context) {
	if w == nil || w.repository == nil || w.wallets == nil {
		return
	}
	if w.interval <= 0 {
		w.interval = 5 * time.Second
	}
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "wallet provisioning cycle failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *WalletProvisioningWorker) RunOnce(ctx context.Context) error {
	jobs, err := w.repository.LeaseWalletProvisioningJobs(ctx, 20, 30*time.Second)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if !job.LeaseUntil.After(time.Now()) {
			continue
		}
		itemCtx, cancel := context.WithDeadline(ctx, job.LeaseUntil)
		var walletErr error
		if job.RegistrationBonusMode == "EVALUATE" {
			provisioner, ok := w.wallets.(RegistrationBonusWalletProvisioner)
			if !ok || job.RegistrationOperationID == nil || job.FingerprintKeyID == "" || len(job.MobileFingerprint) != 32 {
				walletErr = ErrWalletProvisioning
			} else {
				walletErr = provisioner.ProvisionRegistrationWallet(itemCtx, RegistrationWalletProvisionRequest{UserID: job.UserID, RegistrationOperationID: *job.RegistrationOperationID, FingerprintKeyID: job.FingerprintKeyID, MobileFingerprint: hex.EncodeToString(job.MobileFingerprint)})
			}
		} else {
			walletErr = w.wallets.EnsurePersonalWallet(itemCtx, job.UserID)
		}
		cancel()
		if walletErr != nil {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			failErr := w.repository.FailWalletProvisioning(cleanupCtx, job, "BILLING_UNAVAILABLE")
			cleanupCancel()
			if failErr != nil && !errors.Is(failErr, ErrConflict) {
				return failErr
			}
			continue
		}
		if err := w.repository.CompleteWalletProvisioning(ctx, job); err != nil && !errors.Is(err, ErrConflict) {
			return err
		}
	}
	return nil
}

func (r *PostgresRepository) LeaseWalletProvisioningJobs(ctx context.Context, limit int, lease time.Duration) ([]WalletProvisioningJob, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	rows, err := r.pool.Query(ctx, `
WITH candidates AS (
 SELECT user_id FROM wallet_provisioning_jobs
 WHERE (status IN ('PENDING','RETRY') AND next_attempt_at<=$1) OR (status='LEASED' AND lease_until<=$1)
 ORDER BY next_attempt_at,created_at FOR UPDATE SKIP LOCKED LIMIT $2
)
UPDATE wallet_provisioning_jobs j SET status='LEASED',attempts=attempts+1,lease_until=$1::timestamptz+$3::interval,updated_at=$1
FROM candidates c WHERE j.user_id=c.user_id
RETURNING j.user_id,j.registration_operation_id,j.policy_version,j.sla_expires_at,j.attempts,j.lease_until,j.registration_bonus_mode,COALESCE(j.registration_bonus_fingerprint_key_id,''),j.registration_bonus_fingerprint`, r.now(), limit, lease.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := make([]WalletProvisioningJob, 0, limit)
	for rows.Next() {
		var job WalletProvisioningJob
		if err := rows.Scan(&job.UserID, &job.RegistrationOperationID, &job.PolicyVersion, &job.SLAExpiresAt, &job.Attempts, &job.LeaseUntil, &job.RegistrationBonusMode, &job.FingerprintKeyID, &job.MobileFingerprint); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (r *PostgresRepository) CompleteWalletProvisioning(ctx context.Context, job WalletProvisioningJob) error {
	userID := job.UserID
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var operationID *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT registration_operation_id FROM wallet_provisioning_jobs WHERE user_id=$1 AND status='LEASED' AND attempts=$2 AND lease_until=$3 AND lease_until>$4 FOR UPDATE`, userID, job.Attempts, job.LeaseUntil, r.now()).Scan(&operationID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConflict
		}
		return err
	}
	now := r.now()
	var userStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&userStatus); err != nil {
		return err
	}
	if !job.LeaseUntil.After(r.now()) {
		return ErrConflict
	}
	if userStatus != "PROVISIONING" && userStatus != "REPAIR_REQUIRED" && userStatus != "ACTIVE" {
		return ErrConflict
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET status='ACTIVE',version=version+1,updated_at=$2 WHERE id=$1 AND status IN ('PROVISIONING','REPAIR_REQUIRED')`, userID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE wallet_provisioning_jobs SET status='COMPLETED',lease_until=NULL,completed_at=$2,last_error_code=NULL,registration_bonus_mode='NOT_APPLICABLE',registration_bonus_fingerprint_key_id=NULL,registration_bonus_fingerprint=NULL,updated_at=$2 WHERE user_id=$1`, userID, now); err != nil {
		return err
	}
	if operationID != nil {
		if _, err := tx.Exec(ctx, `UPDATE registration_operations SET status='ACTIVE',next_retry_at=$2,last_error_code=NULL,version=version+1,updated_at=$2 WHERE id=$1 AND committed_user_id=$3`, *operationID, now, userID); err != nil {
			return err
		}
	}
	if err := appendOutbox(ctx, tx, "account.wallet.provisioning.completed.v1", userID, 2, map[string]any{"user_id": userID}); err != nil {
		return err
	}
	if err := appendAudit(ctx, tx, userID, "wallet-provisioning", nil, "account.wallet.provision", "user", userID.String(), "SUCCEEDED", map[string]any{"status": "ACTIVE"}); err != nil {
		return err
	}
	// Later statement lock waits must not let an expired claim commit durable changes.
	if !job.LeaseUntil.After(r.now()) {
		return ErrConflict
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) FailWalletProvisioning(ctx context.Context, job WalletProvisioningJob, code string) error {
	userID := job.UserID
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var operationID *uuid.UUID
	var slaExpiresAt time.Time
	var attempts int
	if err := tx.QueryRow(ctx, `SELECT registration_operation_id,sla_expires_at,attempts FROM wallet_provisioning_jobs WHERE user_id=$1 AND status='LEASED' AND attempts=$2 AND lease_until=$3 AND lease_until>$4 FOR UPDATE`, userID, job.Attempts, job.LeaseUntil, r.now()).Scan(&operationID, &slaExpiresAt, &attempts); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConflict
		}
		return err
	}
	now := r.now()
	if !job.LeaseUntil.After(now) {
		return ErrConflict
	}
	status, nextAttempt := "RETRY", now.Add(registrationRetryDelay(attempts))
	if !slaExpiresAt.After(now) {
		status, nextAttempt = "REPAIR_REQUIRED", now
		if _, err := tx.Exec(ctx, `UPDATE users SET status='REPAIR_REQUIRED',version=version+1,updated_at=$2 WHERE id=$1 AND status='PROVISIONING'`, userID, now); err != nil {
			return err
		}
		if operationID != nil {
			if _, err := tx.Exec(ctx, `UPDATE registration_operations SET status='REPAIR_REQUIRED',next_retry_at=$2,last_error_code=$3,version=version+1,updated_at=$2 WHERE id=$1 AND committed_user_id=$4`, *operationID, now, code, userID); err != nil {
				return err
			}
		}
	}
	_, err = tx.Exec(ctx, `UPDATE wallet_provisioning_jobs SET status=$2,next_attempt_at=$3,lease_until=NULL,last_error_code=$4,updated_at=$5 WHERE user_id=$1`, userID, status, nextAttempt, code, now)
	if err != nil {
		return err
	}
	if status == "REPAIR_REQUIRED" {
		if err := appendAudit(ctx, tx, userID, "wallet-provisioning", nil, "account.wallet.provision", "user", userID.String(), "FAILED", map[string]any{"status": status, "reason": code}); err != nil {
			return err
		}
	}
	// Later statement lock waits must not let an expired claim commit durable changes.
	if !job.LeaseUntil.After(r.now()) {
		return ErrConflict
	}
	return tx.Commit(ctx)
}
