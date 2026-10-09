package account

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Call only while holding the same user's row lock used by profile creation
// and SMS verification. State and audit commit with the password operation.
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
