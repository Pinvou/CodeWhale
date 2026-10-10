package account

import (
	"context"
	"errors"
	"time"
)

var ErrInvalidAccountClosureRequest = errors.New("invalid account closure request")

// Renew only the live, same-version claim before another bounded external call.
// A completed/expired/replaced claim can never acquire a new lease this way.
// No database lock is held while calling another service.
func (r *PostgresRepository) RenewAccountClosureClaim(ctx context.Context, item *AccountClosureRequest) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = lockAccountClosureClaim(ctx, tx, *item); err != nil {
		return err
	}
	var until time.Time
	err = tx.QueryRow(ctx, `UPDATE account_closure_requests SET lease_until=clock_timestamp()+interval '30 seconds' WHERE id=$1 RETURNING lease_until`, item.ID).Scan(&until)
	if err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	item.LeaseUntil = &until
	return nil
}
