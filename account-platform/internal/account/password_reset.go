package account

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type PasswordResetOperation struct {
	ID, UserID      uuid.UUID
	Subject, Status string
}

func passwordResetFingerprint(mobile string, challenge uuid.UUID, handle, policy string) [32]byte {
	return sha256.Sum256([]byte(strings.Join([]string{mobile, challenge.String(), handle, policy, "password-reset-v1"}, "\x00")))
}

func scanPasswordReset(row pgx.Row, issuer, mobile string, fingerprint []byte) (PasswordResetOperation, error) {
	var operation PasswordResetOperation
	var storedIssuer, storedMobile string
	var storedFingerprint []byte
	err := row.Scan(&operation.ID, &operation.UserID, &operation.Subject, &operation.Status, &storedIssuer, &storedMobile, &storedFingerprint)
	if errors.Is(err, pgx.ErrNoRows) {
		return PasswordResetOperation{}, ErrNotFound
	}
	if err != nil {
		return PasswordResetOperation{}, err
	}
	if storedIssuer != issuer || storedMobile != mobile || !hmac.Equal(storedFingerprint, fingerprint) {
		return PasswordResetOperation{}, ErrProfileChangeConflict
	}
	return operation, nil
}

const passwordResetSelect = `SELECT pr.id,pr.user_id,pr.identity_subject,pr.status,pr.identity_issuer,pr.mobile_e164,pr.request_fingerprint FROM password_reset_operations pr JOIN users u ON u.id=pr.user_id AND u.status='ACTIVE'`

func (r *PostgresRepository) PasswordResetOperation(ctx context.Context, challenge uuid.UUID, issuer, mobile string, fingerprint []byte) (PasswordResetOperation, error) {
	return scanPasswordReset(r.pool.QueryRow(ctx, passwordResetSelect+` WHERE pr.sms_challenge_id=$1`, challenge), issuer, mobile, fingerprint)
}

// The challenge is consumed in the same transaction that saves the exact reset
// identity. A lost HTTP reply resumes that identity, never a second reset.
func (r *PostgresRepository) CreatePasswordReset(ctx context.Context, issuer, subject string, challenge uuid.UUID, mobile string, fingerprint []byte, verified bool) (PasswordResetOperation, error) {
	if len(fingerprint) != sha256.Size || challenge == uuid.Nil {
		return PasswordResetOperation{}, ErrProfileChangeConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return PasswordResetOperation{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "password-reset:"+challenge.String()); err != nil {
		return PasswordResetOperation{}, err
	}
	prior, err := scanPasswordReset(tx.QueryRow(ctx, passwordResetSelect+` WHERE pr.sms_challenge_id=$1 FOR UPDATE OF pr`, challenge), issuer, mobile, fingerprint)
	if err == nil {
		return prior, tx.Commit(ctx)
	}
	if !errors.Is(err, ErrNotFound) {
		return PasswordResetOperation{}, err
	}
	var userID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT u.id FROM identities i JOIN users u ON u.id=i.user_id AND u.status='ACTIVE' WHERE i.issuer=$1 AND i.subject=$2 AND u.mobile_e164=$3 AND u.mobile_verified_at IS NOT NULL FOR UPDATE OF u`, issuer, subject, mobile).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return PasswordResetOperation{}, ErrSMSInvalid
	}
	if err != nil {
		return PasswordResetOperation{}, err
	}
	var status string
	var attempts int
	var expiry time.Time
	err = tx.QueryRow(ctx, `SELECT status,attempts,expires_at FROM sms_challenges WHERE id=$1 AND mobile_e164=$2 AND purpose='PASSWORD_RESET' FOR UPDATE`, challenge, mobile).Scan(&status, &attempts, &expiry)
	if errors.Is(err, pgx.ErrNoRows) {
		return PasswordResetOperation{}, ErrSMSInvalid
	}
	if err != nil {
		return PasswordResetOperation{}, err
	}
	if status != "SENT" || attempts >= 5 || !expiry.After(r.now()) {
		return PasswordResetOperation{}, ErrSMSInvalid
	}
	now := r.now()
	if !verified {
		_, err = tx.Exec(ctx, `UPDATE sms_challenges SET attempts=attempts+1,status=CASE WHEN attempts+1>=5 THEN 'FAILED' ELSE status END,updated_at=$2 WHERE id=$1`, challenge, now)
		if err != nil {
			return PasswordResetOperation{}, err
		}
		if err = tx.Commit(ctx); err != nil {
			return PasswordResetOperation{}, err
		}
		return PasswordResetOperation{}, ErrSMSInvalid
	}
	operation := PasswordResetOperation{ID: uuid.Must(uuid.NewV7()), UserID: userID, Subject: subject, Status: "VERIFIED"}
	if _, err = tx.Exec(ctx, `UPDATE sms_challenges SET status='CONSUMED',consumed_at=$2,updated_at=$2 WHERE id=$1`, challenge, now); err != nil {
		return PasswordResetOperation{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO password_reset_operations(id,user_id,identity_issuer,identity_subject,sms_challenge_id,mobile_e164,request_fingerprint,status,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,'VERIFIED',$8)`, operation.ID, userID, issuer, subject, challenge, mobile, fingerprint, now); err != nil {
		return PasswordResetOperation{}, err
	}
	if err = appendAudit(ctx, tx, userID, "", nil, "account.password_reset.verified", "password_reset", operation.ID.String(), "SUCCEEDED", map[string]any{"method": "SMS"}); err != nil {
		return PasswordResetOperation{}, err
	}
	return operation, tx.Commit(ctx)
}

func (r *PostgresRepository) CompletePasswordReset(ctx context.Context, id uuid.UUID, code string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var userID uuid.UUID
	var status string
	err = tx.QueryRow(ctx, `SELECT user_id,status FROM password_reset_operations WHERE id=$1 FOR UPDATE`, id).Scan(&userID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if status == "COMPLETED" {
		return tx.Commit(ctx)
	}
	if status != "VERIFIED" {
		return ErrProfileChangeConflict
	}
	if code != "" {
		// A transport failure may hide a completed remote password update. Keep
		// the same operation retryable so Identity can finish session revocation.
		if _, err = tx.Exec(ctx, `UPDATE password_reset_operations SET last_error_code=$2 WHERE id=$1`, id, code); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if _, err = tx.Exec(ctx, `UPDATE password_reset_operations SET status='COMPLETED',last_error_code=NULL,completed_at=$2 WHERE id=$1`, id, r.now()); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM user_sessions WHERE user_id=$1`, userID); err != nil {
		return err
	}
	if err = appendAudit(ctx, tx, userID, "", nil, "account.password_reset.completed", "password_reset", id.String(), "SUCCEEDED", nil); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
