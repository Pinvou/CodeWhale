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
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return RegistrationBonusEligibilityReadiness{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('registration-bonus-eligibility-backfill'))`); err != nil {
		return RegistrationBonusEligibilityReadiness{}, err
	}
	keyVerifier, err := r.registrationBonusFingerprintKeyVerifier()
	if err != nil {
		return RegistrationBonusEligibilityReadiness{}, err
	}
	readiness, err := scanRegistrationBonusEligibilityReadiness(tx.QueryRow(ctx, `SELECT status,COALESCE(fingerprint_key_id,''),recoverable_registration_count,unrecoverable_registration_count,completed_at,COALESCE(fingerprint_key_verifier,'\x'::bytea) FROM registration_bonus_eligibility_state WHERE singleton=TRUE FOR UPDATE`))
	if err != nil {
		return RegistrationBonusEligibilityReadiness{}, err
	}
	if readiness.Status != "PENDING" {
		if readiness.FingerprintKeyID != r.registrationBonusFingerprintKeyID || subtle.ConstantTimeCompare(readiness.keyVerifier, keyVerifier) != 1 {
			return RegistrationBonusEligibilityReadiness{}, errors.New("registration bonus fingerprint key differs from completed eligibility backfill")
		}
		return readiness, tx.Commit(ctx)
	}
	var recoverable int64
	type historicalMobile struct {
		userID uuid.UUID
		mobile string
	}
	cursorMobile, cursorUserID := "", uuid.Nil
	for {
		rows, queryErr := tx.Query(ctx, `SELECT user_id,mobile_e164 FROM (
SELECT DISTINCT ON (mobile_e164) user_id,mobile_e164 FROM (
  SELECT id AS user_id,mobile_e164 FROM users WHERE mobile_verified_at IS NOT NULL AND mobile_e164 LIKE '+86%'
  UNION ALL
  SELECT committed_user_id,mobile_e164 FROM registration_operations WHERE committed_user_id IS NOT NULL AND mobile_e164 LIKE '+86%'
) candidates ORDER BY mobile_e164,user_id
) recoverable
WHERE $1='' OR (mobile_e164,user_id)>($1,$2)
ORDER BY mobile_e164,user_id LIMIT 1000`, cursorMobile, cursorUserID)
		if queryErr != nil {
			return RegistrationBonusEligibilityReadiness{}, queryErr
		}
		batch := make([]historicalMobile, 0, 1000)
		for rows.Next() {
			var item historicalMobile
			if err = rows.Scan(&item.userID, &item.mobile); err != nil {
				rows.Close()
				return RegistrationBonusEligibilityReadiness{}, err
			}
			batch = append(batch, item)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return RegistrationBonusEligibilityReadiness{}, err
		}
		rows.Close()
		for _, item := range batch {
			_, fingerprint, fingerprintErr := r.registrationBonusFingerprint(item.mobile)
			if fingerprintErr != nil {
				return RegistrationBonusEligibilityReadiness{}, fingerprintErr
			}
			if _, err = tx.Exec(ctx, `INSERT INTO registration_bonus_mobile_eligibility(mobile_fingerprint,fingerprint_key_id,first_user_id,source,created_at) VALUES($1,$2,$3,'HISTORICAL_BACKFILL',$4) ON CONFLICT(mobile_fingerprint) DO NOTHING`, fingerprint, r.registrationBonusFingerprintKeyID, item.userID, r.now()); err != nil {
				return RegistrationBonusEligibilityReadiness{}, err
			}
			recoverable++
		}
		if len(batch) < 1000 {
			break
		}
		cursorMobile, cursorUserID = batch[len(batch)-1].mobile, batch[len(batch)-1].userID
	}
	var unrecoverable int64
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM registration_operations WHERE committed_user_id IS NOT NULL AND mobile_e164 NOT LIKE '+86%'`).Scan(&unrecoverable); err != nil {
		return RegistrationBonusEligibilityReadiness{}, err
	}
	status := "READY"
	if unrecoverable > 0 {
		status = "BLOCKED"
	}
	now := r.now()
	if _, err = tx.Exec(ctx, `UPDATE registration_bonus_eligibility_state SET status=$1,fingerprint_key_id=$2,fingerprint_key_verifier=$3,recoverable_registration_count=$4,unrecoverable_registration_count=$5,completed_at=$6,updated_at=$6 WHERE singleton=TRUE`, status, r.registrationBonusFingerprintKeyID, keyVerifier, recoverable, unrecoverable, now); err != nil {
		return RegistrationBonusEligibilityReadiness{}, err
	}
	readiness = RegistrationBonusEligibilityReadiness{Status: status, FingerprintKeyID: r.registrationBonusFingerprintKeyID, RecoverableRegistrationCount: recoverable, UnrecoverableRegistrationCount: unrecoverable, CompletedAt: &now}
	return readiness, tx.Commit(ctx)
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
