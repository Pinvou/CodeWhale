package account

import (
	"context"
	"log/slog"
	"time"
)

// CleanupMobileRecords implements the approved 24-hour captcha / 90-day SMS
// retention policy. Each pass is bounded; database FK locks protect races with
// new business references. Referenced or uncertain deliveries are retained.
// Expired preparation/captcha facts remain; only their raw mobiles are redacted
// after the original capability deadline plus 24 hours, without inferring users.
func (r *PostgresRepository) CleanupMobileRecords(ctx context.Context) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	queries := []string{
		`WITH candidates AS MATERIALIZED (
            SELECT a.id,a.captcha_id FROM password_preparation_authorizations a
            JOIN slider_captcha_challenges c ON c.id=a.captcha_id
            WHERE a.expires_at<=now()-interval '24 hours' AND c.expires_at<=now()-interval '24 hours'
              AND (a.mobile_e164 LIKE '+%' OR c.mobile_e164 LIKE '+%')
            ORDER BY a.expires_at,a.id LIMIT 500 FOR UPDATE OF a,c SKIP LOCKED
        ), redacted AS (
            UPDATE password_preparation_authorizations a
            SET mobile_e164='expired:'||a.id::text FROM candidates
            WHERE a.id=candidates.id RETURNING a.captcha_id
        ) UPDATE slider_captcha_challenges c SET mobile_e164='expired:'||c.id::text,updated_at=now()
          FROM redacted WHERE c.id=redacted.captcha_id`,
		`DELETE FROM slider_captcha_challenges WHERE id IN (
            SELECT c.id FROM slider_captcha_challenges c
            WHERE c.expires_at < now() AND c.updated_at < now()-interval '24 hours'
              AND NOT EXISTS (SELECT 1 FROM password_preparation_authorizations a WHERE a.captcha_id=c.id)
            ORDER BY c.updated_at LIMIT 500 FOR UPDATE OF c SKIP LOCKED)`,
		`DELETE FROM sms_challenges WHERE id IN (
            SELECT c.id FROM sms_challenges c
            WHERE c.expires_at < now() AND c.updated_at < now()-interval '90 days'
              AND (c.delivery_lease_until IS NULL OR c.delivery_lease_until < now())
              AND c.delivery_uncertain_at IS NULL
              AND c.registration_operation_id IS NULL
              AND NOT EXISTS (SELECT 1 FROM registration_operations o WHERE o.sms_challenge_id=c.id)
              AND NOT EXISTS (SELECT 1 FROM profile_change_requests p WHERE p.sms_challenge_id=c.id)
              AND NOT EXISTS (SELECT 1 FROM password_reset_operations p WHERE p.sms_challenge_id=c.id)
            ORDER BY c.updated_at LIMIT 500 FOR UPDATE OF c SKIP LOCKED)`,
		`DELETE FROM sms_send_attempts WHERE id IN (
            SELECT a.id FROM sms_send_attempts a
            WHERE a.occurred_at < now()-interval '90 days'
              AND a.challenge_id IS NULL
            ORDER BY a.occurred_at LIMIT 500 FOR UPDATE SKIP LOCKED)`,
		`DELETE FROM sms_verification_attempts WHERE id IN (
            SELECT a.id FROM sms_verification_attempts a
            WHERE a.occurred_at < now()-interval '90 days'
              AND a.challenge_id IS NULL
            ORDER BY a.occurred_at LIMIT 500 FOR UPDATE SKIP LOCKED)`,
	}
	for _, query := range queries {
		if _, err := tx.Exec(ctx, query); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

type mobileRetentionStore interface{ CleanupMobileRecords(context.Context) error }

type MobileRetentionWorker struct {
	store    mobileRetentionStore
	interval time.Duration
}

func NewMobileRetentionWorker(store mobileRetentionStore, interval time.Duration) *MobileRetentionWorker {
	if interval <= 0 {
		interval = time.Hour
	}
	return &MobileRetentionWorker{store: store, interval: interval}
}

func (w *MobileRetentionWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		pass, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := w.store.CleanupMobileRecords(pass)
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "mobile retention cleanup failed", "error_code", "MOBILE_RETENTION_FAILED")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
