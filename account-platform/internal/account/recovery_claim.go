package account

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var errRecoveryClaimLost = errors.New("recovery claim is no longer current")

type recoveryClaimKey struct{}
type recoveryClaim struct {
	kind       string
	id         uuid.UUID
	generation int64
	until      time.Time
}

func withRecoveryClaim(ctx context.Context, kind string, id uuid.UUID, generation int64, until time.Time) (context.Context, context.CancelFunc) {
	ctx = context.WithValue(ctx, recoveryClaimKey{}, recoveryClaim{kind, id, generation, until})
	return context.WithDeadline(ctx, until)
}

// A worker's detached cleanup keeps this claim in its context. The row lock
// makes the generation check and its local write one atomic database decision.
func (r *PostgresRepository) lockRecoveryClaim(ctx context.Context, tx pgx.Tx, kind string, id uuid.UUID) error {
	claim, claimed := ctx.Value(recoveryClaimKey{}).(recoveryClaim)
	if !claimed {
		return nil
	}
	if claim.kind != kind || claim.id != id || !claim.until.After(r.now()) {
		return errRecoveryClaimLost
	}
	var generation int64
	var until *time.Time
	var query string
	switch kind {
	case "registration":
		query = `SELECT recovery_generation,recovery_lease_until FROM registration_operations WHERE id=$1 FOR UPDATE`
	case "profile":
		query = `SELECT attempts,lease_until FROM profile_change_requests WHERE id=$1 FOR UPDATE`
	default:
		return errRecoveryClaimLost
	}
	if err := tx.QueryRow(ctx, query, id).Scan(&generation, &until); err != nil {
		return err
	}
	if until == nil || generation != claim.generation || !until.Equal(claim.until) || !until.After(r.now()) {
		return errRecoveryClaimLost
	}
	return nil
}
