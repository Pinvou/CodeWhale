package account

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type RegistrationBonusEligibilityReadiness struct {
	Status                         string     `json:"status"`
	FingerprintKeyID               string     `json:"fingerprint_key_id,omitempty"`
	RecoverableRegistrationCount   int64      `json:"recoverable_registration_count"`
	UnrecoverableRegistrationCount int64      `json:"unrecoverable_registration_count"`
	CompletedAt                    *time.Time `json:"completed_at,omitempty"`
	keyVerifier                    []byte
}

// PrepareRegistrationBonusEligibility builds the permanent first-registration
// set before the service starts accepting new registrations. Historical rows
// anonymized before this feature cannot be reconstructed, so readiness remains
// BLOCKED instead of silently treating those mobiles as new.
func (r *PostgresRepository) PrepareRegistrationBonusEligibility(ctx context.Context) (RegistrationBonusEligibilityReadiness, error) {
	if r.registrationBonusFingerprintKeyID == "" || len(r.registrationBonusFingerprintKey) != 32 {
		return RegistrationBonusEligibilityReadiness{}, errors.New("registration bonus fingerprint is not configured")
	}
	keyVerifier, err := r.registrationBonusFingerprintKeyVerifier()
	if err != nil {
		return RegistrationBonusEligibilityReadiness{}, err
	}
	for {
		tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			return RegistrationBonusEligibilityReadiness{}, err
		}
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('registration-bonus-eligibility-backfill'))`); err != nil {
			tx.Rollback(ctx)
			return RegistrationBonusEligibilityReadiness{}, err
		}
		var readiness RegistrationBonusEligibilityReadiness
		var cursorMobile *string
		var cursorUserID *uuid.UUID
		var snapshotAt *time.Time
		err = tx.QueryRow(ctx, `SELECT status,COALESCE(fingerprint_key_id,''),recoverable_registration_count,unrecoverable_registration_count,completed_at,COALESCE(fingerprint_key_verifier,'\x'::bytea),backfill_cursor_mobile_e164,backfill_cursor_user_id,backfill_snapshot_at FROM registration_bonus_eligibility_state WHERE singleton=TRUE FOR UPDATE`).Scan(&readiness.Status, &readiness.FingerprintKeyID, &readiness.RecoverableRegistrationCount, &readiness.UnrecoverableRegistrationCount, &readiness.CompletedAt, &readiness.keyVerifier, &cursorMobile, &cursorUserID, &snapshotAt)
		if err != nil {
			tx.Rollback(ctx)
			return RegistrationBonusEligibilityReadiness{}, err
		}
		if readiness.Status != "PENDING" || readiness.FingerprintKeyID != "" {
			if readiness.FingerprintKeyID != r.registrationBonusFingerprintKeyID || subtle.ConstantTimeCompare(readiness.keyVerifier, keyVerifier) != 1 {
				tx.Rollback(ctx)
				return RegistrationBonusEligibilityReadiness{}, errors.New("registration bonus fingerprint key differs from eligibility backfill")
			}
		}
		if readiness.Status != "PENDING" {
			if err := tx.Commit(ctx); err != nil {
				return RegistrationBonusEligibilityReadiness{}, err
			}
			return readiness, nil
		}
		if readiness.FingerprintKeyID == "" {
			// A committed prefix without its verifier cannot be safely attributed
			// to the configured key, even when its key ID happens to match.
			var existingFacts bool
			if readiness.RecoverableRegistrationCount != 0 || cursorMobile != nil || cursorUserID != nil || snapshotAt != nil {
				tx.Rollback(ctx)
				return RegistrationBonusEligibilityReadiness{}, errors.New("registration bonus backfill progress has no fingerprint key binding")
			}
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM registration_bonus_mobile_eligibility)`).Scan(&existingFacts); err != nil {
				tx.Rollback(ctx)
				return RegistrationBonusEligibilityReadiness{}, err
			}
			if existingFacts {
				tx.Rollback(ctx)
				return RegistrationBonusEligibilityReadiness{}, errors.New("registration bonus eligibility facts have no fingerprint key binding")
			}
		}
		type historicalMobile struct {
			userID uuid.UUID
			mobile string
		}
		mobileCursor, userCursor := "", uuid.Nil
		if cursorMobile != nil {
			mobileCursor = *cursorMobile
		}
		if cursorUserID != nil {
			userCursor = *cursorUserID
		}
		candidateSnapshot := r.now()
		if snapshotAt != nil {
			candidateSnapshot = *snapshotAt
		}
		rows, queryErr := tx.Query(ctx, `SELECT user_id,mobile_e164 FROM (
SELECT DISTINCT ON (mobile_e164) user_id,mobile_e164 FROM (
  SELECT id AS user_id,mobile_e164 FROM users WHERE mobile_verified_at IS NOT NULL AND mobile_verified_at <= $3 AND mobile_e164 LIKE '+86%' AND created_at <= $3
  UNION ALL
  SELECT committed_user_id,mobile_e164 FROM registration_operations WHERE committed_user_id IS NOT NULL AND account_committed_at IS NOT NULL AND account_committed_at <= $3 AND mobile_e164 LIKE '+86%' AND created_at <= $3
) candidates ORDER BY mobile_e164,user_id
) recoverable
		WHERE $1='' OR (mobile_e164,user_id)>($1,$2)
		ORDER BY mobile_e164,user_id LIMIT 1000`, mobileCursor, userCursor, candidateSnapshot)
		if queryErr != nil {
			tx.Rollback(ctx)
			return RegistrationBonusEligibilityReadiness{}, queryErr
		}
		batch := make([]historicalMobile, 0, 1000)
		for rows.Next() {
			var item historicalMobile
			if err = rows.Scan(&item.userID, &item.mobile); err != nil {
				rows.Close()
				tx.Rollback(ctx)
				return RegistrationBonusEligibilityReadiness{}, err
			}
			batch = append(batch, item)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			tx.Rollback(ctx)
			return RegistrationBonusEligibilityReadiness{}, err
		}
		rows.Close()
		for _, item := range batch {
			_, fingerprint, fingerprintErr := r.registrationBonusFingerprint(item.mobile)
			if fingerprintErr != nil {
				tx.Rollback(ctx)
				return RegistrationBonusEligibilityReadiness{}, fingerprintErr
			}
			if _, err = tx.Exec(ctx, `INSERT INTO registration_bonus_mobile_eligibility(mobile_fingerprint,fingerprint_key_id,first_user_id,source,created_at) VALUES($1,$2,$3,'HISTORICAL_BACKFILL',$4) ON CONFLICT(mobile_fingerprint) DO NOTHING`, fingerprint, r.registrationBonusFingerprintKeyID, item.userID, r.now()); err != nil {
				tx.Rollback(ctx)
				return RegistrationBonusEligibilityReadiness{}, err
			}
		}
		if len(batch) == 1000 {
			last := batch[len(batch)-1]
			if _, err = tx.Exec(ctx, `UPDATE registration_bonus_eligibility_state SET recoverable_registration_count=recoverable_registration_count+$1,backfill_cursor_mobile_e164=$2,backfill_cursor_user_id=$3,backfill_snapshot_at=$4,updated_at=$5,fingerprint_key_id=$6,fingerprint_key_verifier=$7 WHERE singleton=TRUE AND status='PENDING'`, len(batch), last.mobile, last.userID, candidateSnapshot, r.now(), r.registrationBonusFingerprintKeyID, keyVerifier); err != nil {
				tx.Rollback(ctx)
				return RegistrationBonusEligibilityReadiness{}, err
			}
			if err := tx.Commit(ctx); err != nil {
				return RegistrationBonusEligibilityReadiness{}, err
			}
			continue
		}
		var unrecoverable int64
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM registration_operations WHERE committed_user_id IS NOT NULL AND mobile_e164 NOT LIKE '+86%' AND created_at <= $1 AND (account_committed_at IS NULL OR account_committed_at <= $1)`, candidateSnapshot).Scan(&unrecoverable); err != nil {
			tx.Rollback(ctx)
			return RegistrationBonusEligibilityReadiness{}, err
		}
		var mismatchedKey bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM registration_bonus_mobile_eligibility WHERE fingerprint_key_id <> $1)`, r.registrationBonusFingerprintKeyID).Scan(&mismatchedKey); err != nil {
			tx.Rollback(ctx)
			return RegistrationBonusEligibilityReadiness{}, err
		}
		if mismatchedKey {
			tx.Rollback(ctx)
			return RegistrationBonusEligibilityReadiness{}, errors.New("registration bonus eligibility facts use a different fingerprint key")
		}
		status := "READY"
		if unrecoverable > 0 {
			status = "BLOCKED"
		}
		now := r.now()
		if _, err = tx.Exec(ctx, `UPDATE registration_bonus_eligibility_state SET status=$1,fingerprint_key_id=$2,fingerprint_key_verifier=$3,recoverable_registration_count=recoverable_registration_count+$4,unrecoverable_registration_count=$5,backfill_cursor_mobile_e164=NULL,backfill_cursor_user_id=NULL,backfill_snapshot_at=NULL,completed_at=$6,updated_at=$6 WHERE singleton=TRUE AND status='PENDING'`, status, r.registrationBonusFingerprintKeyID, keyVerifier, len(batch), unrecoverable, now); err != nil {
			tx.Rollback(ctx)
			return RegistrationBonusEligibilityReadiness{}, err
		}
		readiness.FingerprintKeyID = r.registrationBonusFingerprintKeyID
		readiness.keyVerifier = keyVerifier
		readiness.Status = status
		readiness.RecoverableRegistrationCount += int64(len(batch))
		readiness.UnrecoverableRegistrationCount = unrecoverable
		readiness.CompletedAt = &now
		if err := tx.Commit(ctx); err != nil {
			return RegistrationBonusEligibilityReadiness{}, err
		}
		return readiness, nil
	}
}

func (r *PostgresRepository) RegistrationBonusEligibilityReadiness(ctx context.Context) (RegistrationBonusEligibilityReadiness, error) {
	return scanRegistrationBonusEligibilityReadiness(r.pool.QueryRow(ctx, `SELECT status,COALESCE(fingerprint_key_id,''),recoverable_registration_count,unrecoverable_registration_count,completed_at,COALESCE(fingerprint_key_verifier,'\x'::bytea) FROM registration_bonus_eligibility_state WHERE singleton=TRUE`))
}

func scanRegistrationBonusEligibilityReadiness(row pgx.Row) (RegistrationBonusEligibilityReadiness, error) {
	var readiness RegistrationBonusEligibilityReadiness
	err := row.Scan(&readiness.Status, &readiness.FingerprintKeyID, &readiness.RecoverableRegistrationCount, &readiness.UnrecoverableRegistrationCount, &readiness.CompletedAt, &readiness.keyVerifier)
	return readiness, err
}

func (s *Service) internalRegistrationBonusEligibilityReadiness(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	repository, ok := s.repository.(interface {
		RegistrationBonusEligibilityReadiness(context.Context) (RegistrationBonusEligibilityReadiness, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "REGISTRATION_BONUS_ELIGIBILITY_UNAVAILABLE", "registration bonus eligibility readiness is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	readiness, err := repository.RegistrationBonusEligibilityReadiness(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, readiness)
}

func registrationBonusMode(status string, inserted bool) string {
	if strings.EqualFold(status, "READY") && inserted {
		return "EVALUATE"
	}
	return "NOT_APPLICABLE"
}
