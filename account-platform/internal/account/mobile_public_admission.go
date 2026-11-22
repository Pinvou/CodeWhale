package account

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// AdmitPublicMobileRequest bounds unauthenticated checks and image generation
// across processes and restarts. Device IDs are not trusted admission keys.
// The shared global gate runs before arbitrary target/source bucket creation,
// limiting cardinality to at most 6600 active buckets plus one global row
// (ten-minute TTL and at most eleven overlapping global minute windows).
func (r *PostgresRepository) AdmitPublicMobileRequest(ctx context.Context, operation, rawMobile, source string) error {
	if operation != "PRECHECK" && operation != "CAPTCHA" {
		return ErrConflict
	}
	mobile, err := NormalizeMainlandMobile(rawMobile)
	if err != nil {
		mobile = "invalid"
	}
	source = strings.TrimSpace(source)
	if source == "" || len(source) > 256 {
		source = "source:unknown"
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Serialize all public admission before reading the database clock. This
	// also serializes rollover cleanup with other bucket readers/writers.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('public-mobile-admission-v1',0))`); err != nil {
		return err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	globalKey := sha256.Sum256([]byte("global"))
	globalExpiry, count, err := publicMobileBucket(ctx, tx, "GLOBAL", globalKey[:], now)
	if err != nil {
		return err
	}
	if count >= 300 {
		return &SMSRateLimitError{RetryAfter: max(time.Second, globalExpiry.Sub(now))}
	}
	// Only a new global minute sweeps expired, unreferenced counters. No SMS,
	// captcha or registration business record is deleted by this policy.
	if count == 0 {
		if _, err = tx.Exec(ctx, `DELETE FROM mobile_public_admission_buckets WHERE expires_at<=$1`, now); err != nil {
			return err
		}
	}
	type dimension struct {
		scope string
		key   [32]byte
		limit int
	}
	dimensions := []dimension{
		{"SOURCE", sha256.Sum256([]byte(source)), 30},
		{operation + "_MOBILE", sha256.Sum256([]byte(mobile)), 10},
	}
	// Consume global admission even when a target quota is exhausted. Otherwise
	// repeatedly rejected targets could force unlimited database work/cleanup.
	if err = incrementPublicMobileBucket(ctx, tx, "GLOBAL", globalKey[:], now, time.Minute); err != nil {
		return err
	}
	var retry time.Duration
	for _, d := range dimensions {
		expiry, attempts, readErr := publicMobileBucket(ctx, tx, d.scope, d.key[:], now)
		if readErr != nil {
			return readErr
		}
		if attempts >= d.limit {
			retry = max(retry, expiry.Sub(now))
		}
	}
	if retry == 0 {
		for _, d := range dimensions {
			if err = incrementPublicMobileBucket(ctx, tx, d.scope, d.key[:], now, 10*time.Minute); err != nil {
				return err
			}
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	if retry > 0 {
		return &SMSRateLimitError{RetryAfter: max(time.Second, retry)}
	}
	return nil
}

func publicMobileBucket(ctx context.Context, tx pgx.Tx, scope string, key []byte, now time.Time) (time.Time, int, error) {
	var expiry time.Time
	var attempts int
	err := tx.QueryRow(ctx, `SELECT expires_at,attempts FROM mobile_public_admission_buckets WHERE scope=$1 AND bucket_hash=$2`, scope, key).Scan(&expiry, &attempts)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !expiry.After(now) {
		return time.Time{}, 0, nil
	}
	return expiry, attempts, err
}

func incrementPublicMobileBucket(ctx context.Context, tx pgx.Tx, scope string, key []byte, now time.Time, window time.Duration) error {
	_, err := tx.Exec(ctx, `INSERT INTO mobile_public_admission_buckets(scope,bucket_hash,attempts,expires_at)
VALUES($1,$2,1,$3) ON CONFLICT(scope,bucket_hash) DO UPDATE
SET attempts=CASE WHEN mobile_public_admission_buckets.expires_at<=$4 THEN 1 ELSE mobile_public_admission_buckets.attempts+1 END,
expires_at=CASE WHEN mobile_public_admission_buckets.expires_at<=$4 THEN EXCLUDED.expires_at ELSE mobile_public_admission_buckets.expires_at END`, scope, key, now.Add(window), now)
	return err
}
