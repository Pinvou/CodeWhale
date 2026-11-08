package account

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrProfileSessionRevocationUnconfirmed = errors.New("profile session revocation has no confirmed receipt")

// Persist the first-dispatch decision before contacting the adapter. Retrying
// cannot grant a second first attempt, including after either process restarts.
func (r *PostgresRepository) StartProfileSessionRevocation(ctx context.Context, id uuid.UUID) (bool, error) {
	first := false
	err := r.profileRecoveryWrite(ctx, id, func(tx pgx.Tx) error {
		var c ProfileChange
		var err error
		c, err = scanProfileChange(tx.QueryRow(ctx, profileChangeSelect+` WHERE id=$1 FOR UPDATE`, id))
		if err != nil {
			return err
		}
		if c.AccountCommittedAt == nil || c.Status == "CANCELLED_ACCOUNT_CLOSURE" || c.Status == "SUPERSEDED" {
			return ErrProfileChangeConflict
		}
		if c.SessionsRevocationStartedAt != nil || c.SessionsRevokedAt != nil {
			return nil
		}
		if _, err = tx.Exec(ctx, `UPDATE profile_change_requests SET sessions_revocation_started_at=$2,version=version+1,updated_at=$2 WHERE id=$1`, id, r.now()); err != nil {
			return err
		}
		first = true
		return appendAudit(ctx, tx, c.UserID, "", nil, "account.profile_change.revocation_started", "profile_change", id.String(), "SUCCEEDED", map[string]any{"stage": "SESSION_REVOCATION"})
	})
	return first, err
}
