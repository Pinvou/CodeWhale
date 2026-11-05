package account

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"time"
)

// The first grant is durable before the network boundary. Every later caller
// may only look up the original operation, even if it has not appeared yet.
func (r *PostgresRepository) MarkRegistrationCreateAttempted(ctx context.Context, id uuid.UUID) (bool, int64, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, 0, err
	}
	defer tx.Rollback(ctx)
	if err = r.lockIdentityRecoveryClaim(ctx, tx, "registration", id); err != nil {
		return false, 0, err
	}
	var prior *time.Time
	var version int64
	err = tx.QueryRow(ctx, `SELECT identity_create_attempted_at,version FROM registration_operations WHERE id=$1 AND status='IDENTITY_CREATING' AND identity_subject IS NULL AND committed_user_id IS NULL FOR UPDATE`, id).Scan(&prior, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, 0, ErrRegistrationConflict
	}
	if err != nil {
		return false, 0, err
	}
	version++
	if _, err = tx.Exec(ctx, `UPDATE registration_operations SET identity_create_attempted_at=COALESCE(identity_create_attempted_at,$2),version=$3,updated_at=$2 WHERE id=$1`, id, r.now(), version); err != nil {
		return false, 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, 0, err
	}
	return prior == nil, version, nil
}
