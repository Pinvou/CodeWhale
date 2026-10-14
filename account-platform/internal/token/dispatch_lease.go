package token

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	dispatchLeaseDuration        = 10 * time.Minute
	dispatchLeaseRenewalInterval = 4 * time.Minute
	dispatchLeaseWriteTimeout    = 10 * time.Second
)

type dispatchLeaseOwnerKey struct{}

func withDispatchLeaseOwner(ctx context.Context, owner string) context.Context {
	return context.WithValue(ctx, dispatchLeaseOwnerKey{}, owner)
}

// Renewal cannot acquire or resurrect a dispatch. Acquisition is one-shot and
// the envelope was destroyed at its first commit; an expired owner is fenced.
func (r *PostgresRepository) RenewDispatchLease(ctx context.Context, usageID uuid.UUID, owner string) (bool, error) {
	owner = strings.TrimSpace(owner)
	if usageID == uuid.Nil || owner == "" {
		return false, ErrForbidden
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if err := lockReplayUsageTx(ctx, tx, usageID); err != nil {
		return false, err
	}
	var currentOwner *string
	var until, acquired *time.Time
	if err := tx.QueryRow(ctx, `SELECT worker_lease_owner,worker_lease_until,worker_acquired_at FROM usage_dispatch_intents WHERE usage_request_id=$1 FOR UPDATE`, usageID).Scan(&currentOwner, &until, &acquired); err != nil {
		return false, err
	}
	var execution, status, settlement string
	var providerTerminalAt *time.Time
	if err := tx.QueryRow(ctx, `SELECT execution_status,status,settlement_status,provider_terminal_at FROM usage_requests WHERE id=$1 FOR UPDATE`, usageID).Scan(&execution, &status, &settlement, &providerTerminalAt); err != nil {
		return false, err
	}
	if currentOwner == nil || *currentOwner != owner || until == nil || acquired == nil {
		return false, ErrForbidden
	}
	if execution != "UPSTREAM_DISPATCHING" && execution != "STREAMING" {
		// REVIEW is a loss of live ownership, not proof that the blocked
		// provider finished. The loop must cancel it rather than stop quietly.
		// SaveReplayResult records provider termination + result in one tx;
		// a generic MarkReview label alone must not stand in for that receipt.
		if execution == "SUCCEEDED" && status == "COMMITTED" && settlement == "SETTLED" {
			return false, tx.Commit(ctx)
		}
		if (execution == "SETTLEMENT_PENDING" || (execution == "FAILED" && status == "RELEASED")) && providerTerminalAt != nil {
			var responsePersisted bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM usage_replay_results WHERE usage_request_id=$1)`, usageID).Scan(&responsePersisted); err != nil {
				return false, err
			}
			if responsePersisted {
				return false, tx.Commit(ctx)
			}
		}
		return false, ErrForbidden
	}
	now := r.now()
	if !until.After(now) {
		return false, ErrForbidden
	}
	command, err := tx.Exec(ctx, `UPDATE usage_dispatch_intents SET worker_lease_until=GREATEST(worker_lease_until,$2) WHERE usage_request_id=$1 AND worker_lease_owner=$3 AND worker_acquired_at=$4 AND worker_lease_until=$5`, usageID, now.Add(dispatchLeaseDuration), owner, acquired, until)
	if err != nil {
		return false, err
	}
	if command.RowsAffected() != 1 {
		return false, ErrForbidden
	}
	return true, tx.Commit(ctx)
}

// Callers which possess the actual acquired envelope carry its local owner in
// context. Historical replay/audit producers keep their existing contract.
// Each caller already holds the usage row. Lease writers keep Acquire's intent
// -> usage lock order, so this read deliberately does not lock the intent:
// their lease/owner changes cannot commit while this usage row is held.
func (r *PostgresRepository) checkDispatchLeaseOwnerTx(ctx context.Context, tx pgx.Tx, usageID uuid.UUID) error {
	owner, owned := ctx.Value(dispatchLeaseOwnerKey{}).(string)
	if !owned {
		return nil
	}
	if strings.TrimSpace(owner) == "" {
		return ErrForbidden
	}
	var execution string
	if err := tx.QueryRow(ctx, `SELECT execution_status FROM usage_requests WHERE id=$1`, usageID).Scan(&execution); err != nil {
		return err
	}
	var currentOwner *string
	var until, acquired *time.Time
	if err := tx.QueryRow(ctx, `SELECT worker_lease_owner,worker_lease_until,worker_acquired_at FROM usage_dispatch_intents WHERE usage_request_id=$1`, usageID).Scan(&currentOwner, &until, &acquired); err != nil {
		return err
	}
	if currentOwner == nil || *currentOwner != owner || until == nil || acquired == nil {
		return ErrForbidden
	}
	if (execution == "UPSTREAM_DISPATCHING" || execution == "STREAMING" || execution == "REVIEW") && !until.After(r.now()) {
		return ErrForbidden
	}
	return nil
}

// ticks is the production ticker channel, allowing the existing real repository
// and this same service loop to be exercised without waiting two wall-clock hours.
func (s *Service) renewDispatchLease(parent context.Context, cancelUpstream context.CancelFunc, usageID uuid.UUID, owner string, ticks <-chan time.Time) (<-chan struct{}, func(), error) {
	ctx, cancel := context.WithCancel(parent)
	renew := func() (bool, error) {
		writeContext, stop := context.WithTimeout(ctx, dispatchLeaseWriteTimeout)
		defer stop()
		return s.repository.RenewDispatchLease(writeContext, usageID, owner)
	}
	active, err := renew()
	if err != nil || !active {
		cancel()
		if err == nil {
			err = ErrForbidden
		}
		return nil, func() {}, err
	}
	failed := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case _, open := <-ticks:
				if !open || ctx.Err() != nil {
					return
				}
				active, err := renew()
				if err != nil {
					if ctx.Err() == nil {
						close(failed)
						cancelUpstream()
					}
					return
				}
				if !active {
					return
				}
			}
		}
	}()
	return failed, func() { cancel(); <-done }, nil
}
