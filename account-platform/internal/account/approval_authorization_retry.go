package account

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// approvalAuthorizationRetryDelay only schedules a retry; it never owns a
// transaction. The caller must first establish that the entire failed transaction
// was aborted, then start a fresh transaction and recheck all locked state. Never
// call this helper to retry a commit whose outcome is unknown.
// attempt is the number of failed full-transaction attempts (starting at one).
func approvalAuthorizationRetryDelay(ctx context.Context, attempt int, err error, jitter func(time.Duration) time.Duration) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	var postgresErr *pgconn.PgError
	if attempt < 1 || attempt >= 3 || !errors.As(err, &postgresErr) ||
		(postgresErr.Code != "40001" && postgresErr.Code != "40P01") {
		return err
	}
	maximum := time.Duration(attempt) * 25 * time.Millisecond
	delay := time.Duration(0)
	if jitter == nil {
		delay = time.Duration(rand.Int64N(int64(maximum) + 1))
	} else {
		delay = jitter(maximum)
	}
	if delay < 0 {
		delay = 0
	} else if delay > maximum {
		delay = maximum
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

// approvalAuthorizationPublicError keeps database diagnostics (which may
// contain object names or connection details) behind the stable writer error.
func approvalAuthorizationPublicError(ctx context.Context, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	var postgresErr *pgconn.PgError
	var connectErr *pgconn.ConnectError
	if errors.Is(err, pgx.ErrTxCommitRollback) || errors.Is(err, ErrApprovalAuthorizationControlChanged) ||
		errors.Is(err, ErrApprovalAuthorizationInvalid) || errors.As(err, &postgresErr) ||
		errors.As(err, &connectErr) || pgconn.SafeToRetry(err) {
		return ErrApprovalWriterUnavailable
	}
	return err
}

// approvalAuthorizationCommitRolledBack recognizes only commit failures for
// which PostgreSQL/pgx explicitly proves that the transaction was aborted.
// Transport and connection errors are outcome-unknown and must return false.
func approvalAuthorizationCommitRolledBack(err error) bool {
	if errors.Is(err, pgx.ErrTxCommitRollback) {
		return true
	}
	var postgresErr *pgconn.PgError
	return errors.As(err, &postgresErr) && (postgresErr.Code == "40001" || postgresErr.Code == "40P01")
}

func (r *PostgresRepository) commitApprovalWriterTransaction(ctx context.Context, tx pgx.Tx) (committed, rolledBack bool, err error) {
	if r.approvalWriterCommit == nil {
		err = tx.Commit(ctx)
	} else {
		err = r.approvalWriterCommit(ctx, tx)
	}
	if err == nil {
		return true, false, nil
	}
	if approvalAuthorizationCommitRolledBack(err) {
		return false, true, err
	}
	return false, false, ErrApprovalWriterUnavailable
}
