package billing

import (
	"context"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The current service does not issue organization grants. Existing allocation
// facts must never be silently dropped by a personal package expiry.
func ensureNoMemberQuotaGrantsForComponent(ctx context.Context, tx pgx.Tx, componentID uuid.UUID) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM member_quota_grants WHERE component_id=$1)`, componentID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrConflict
	}
	return nil
}
