package account

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Call only while holding the user's row lock shared with profile creation.
// SMS verification serializes on the profile row; the UPDATE proof predicates
// prevent superseding verified work. State and audit share the password transaction.
func supersedeExpiredPendingProfileChanges(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error {
	rows, err := tx.Query(ctx, `UPDATE profile_change_requests
 SET status='SUPERSEDED',version=version+1,last_error_code='EXPIRED_BEFORE_VERIFICATION',updated_at=clock_timestamp()
 WHERE user_id=$1 AND status='PENDING' AND sla_expires_at<=clock_timestamp()
 AND sms_verified_at IS NULL AND identity_updated_at IS NULL
 AND account_committed_at IS NULL AND sessions_revoked_at IS NULL
 RETURNING id,field`, userID)
	if err != nil {
		return err
	}
	type retired struct {
		id    uuid.UUID
		field string
	}
	var changes []retired
	for rows.Next() {
		var item retired
		if err := rows.Scan(&item.id, &item.field); err != nil {
			rows.Close()
			return err
		}
		changes = append(changes, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, item := range changes {
		if err := appendAudit(ctx, tx, userID, "", nil, "account.profile_change.superseded", "profile_change", item.id.String(), "SUCCEEDED", map[string]any{"field": item.field, "reason": "EXPIRED_BEFORE_VERIFICATION"}); err != nil {
			return err
		}
	}
	return nil
}

const expiredMobileSignupReservation = `
request.field='MOBILE' AND request.new_value=$1 AND request.status='PENDING'
AND request.sla_expires_at<=clock_timestamp() AND request.sms_verified_at IS NULL
AND request.identity_updated_at IS NULL AND request.account_committed_at IS NULL
AND request.sessions_revoked_at IS NULL`

// Consented signup holds the phone lock. Do not wait on profile and user
// row locks: their owners may need the phone lock. If retirement is busy,
// the signup transaction rolls back, including consumption of its challenge.
func supersedeExpiredMobileSignupReservation(ctx context.Context, tx pgx.Tx, mobile string) (bool, error) {
	var id, userID uuid.UUID
	err := tx.QueryRow(ctx, `
WITH owner AS MATERIALIZED (
  SELECT u.id FROM users u JOIN profile_change_requests request ON request.user_id=u.id
  WHERE `+expiredMobileSignupReservation+`
  FOR KEY SHARE OF u SKIP LOCKED
), expired AS (
  SELECT request.id FROM profile_change_requests request JOIN owner ON owner.id=request.user_id
  WHERE `+expiredMobileSignupReservation+`
  FOR UPDATE OF request SKIP LOCKED
)
UPDATE profile_change_requests request
SET status='SUPERSEDED',version=request.version+1,last_error_code='EXPIRED_BEFORE_VERIFICATION',updated_at=clock_timestamp()
FROM expired WHERE request.id=expired.id
RETURNING request.id,request.user_id`, mobile).Scan(&id, &userID)
	if errors.Is(err, pgx.ErrNoRows) {
		var busy bool
		err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM profile_change_requests request WHERE `+expiredMobileSignupReservation+`)`, mobile).Scan(&busy)
		if err == nil && busy {
			return false, ErrRegistrationRecovering
		}
		return false, err
	}
	if err != nil {
		return false, err
	}
	if err := appendAudit(ctx, tx, userID, "", nil, "account.profile_change.superseded", "profile_change", id.String(), "SUCCEEDED",
		map[string]any{"field": "MOBILE", "reason": "EXPIRED_BEFORE_VERIFICATION"}); err != nil {
		return false, err
	}
	return true, nil
}
