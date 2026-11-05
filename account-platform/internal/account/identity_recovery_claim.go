package account

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrIdentityRecoveryClaimLost = errors.New("identity recovery claim is no longer current")

type identityRecoveryClaimKey struct{}
type identityRecoveryClaim struct {
	kind       string
	id         uuid.UUID
	generation int64
	until      time.Time
}

// The value survives WithoutCancel cleanup. Every local worker write checks
// ownership again under its database row lock, independently of ctx deadlines.
func withIdentityRecoveryClaim(ctx context.Context, kind string, id uuid.UUID, generation int64, until *time.Time) (context.Context, context.CancelFunc, error) {
	if id == uuid.Nil || generation <= 0 || until == nil || !until.After(time.Now()) {
		return nil, nil, ErrIdentityRecoveryClaimLost
	}
	ctx = context.WithValue(ctx, identityRecoveryClaimKey{}, identityRecoveryClaim{kind, id, generation, *until})
	ctx, cancel := context.WithDeadline(ctx, *until)
	return ctx, cancel, nil
}

func (r *PostgresRepository) lockIdentityRecoveryClaim(ctx context.Context, tx pgx.Tx, kind string, id uuid.UUID) error {
	claim, claimed := ctx.Value(identityRecoveryClaimKey{}).(identityRecoveryClaim)
	if !claimed {
		return nil
	} // Ordinary request paths are not worker leases.
	if claim.kind != kind || claim.id != id || !claim.until.After(r.now()) {
		return ErrIdentityRecoveryClaimLost
	}
	var generation int64
	var until *time.Time
	var query string
	switch kind {
	case "registration":
		query = `SELECT recovery_generation,recovery_lease_until FROM registration_operations WHERE id=$1 FOR UPDATE`
	case "profile":
		query = `SELECT recovery_generation,recovery_lease_until FROM profile_change_requests WHERE id=$1 FOR UPDATE`
	default:
		return ErrIdentityRecoveryClaimLost
	}
	if err := tx.QueryRow(ctx, query, id).Scan(&generation, &until); err != nil {
		return err
	}
	if until == nil || generation != claim.generation || !until.Equal(claim.until) || !until.After(r.now()) {
		return ErrIdentityRecoveryClaimLost
	}
	return nil
}

func (r *PostgresRepository) identityRecoveryWrite(ctx context.Context, kind string, id uuid.UUID, write func(pgx.Tx) error) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = r.lockIdentityRecoveryClaim(ctx, tx, kind, id); err != nil {
		return err
	}
	if err = write(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Audits reference their actor user. Lock USER before profile even when the
// audit FK would otherwise acquire only KEY SHARE after the profile write.
func (r *PostgresRepository) profileRecoveryWrite(ctx context.Context, id uuid.UUID, write func(pgx.Tx) error) error {
	var userID uuid.UUID
	if err := r.pool.QueryRow(ctx, `SELECT user_id FROM profile_change_requests WHERE id=$1`, id).Scan(&userID); err != nil {
		return err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var lockedUser, profileUser uuid.UUID
	if err = tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&lockedUser); err != nil {
		return err
	}
	if err = tx.QueryRow(ctx, `SELECT user_id FROM profile_change_requests WHERE id=$1 FOR UPDATE`, id).Scan(&profileUser); err != nil {
		return err
	}
	if profileUser != lockedUser {
		return ErrConflict
	}
	// In particular, a lease may have expired while waiting for USER or profile.
	if err = r.lockIdentityRecoveryClaim(ctx, tx, "profile", id); err != nil {
		return err
	}
	if err = write(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
