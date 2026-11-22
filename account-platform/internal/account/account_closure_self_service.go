package account

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// The caller locks the original claim first. No row lock crosses a domain RPC.
// Its user snapshot was captured at claim time, before domain assessments; a
// concurrent status/version change therefore cannot be overwritten afterward.
func lockAccountClosurePreCleanupUser(ctx context.Context, tx pgx.Tx, item AccountClosureRequest) error {
	if item.Stage != "CLOSURE_REQUESTED" && item.Stage != "QUIESCING" && item.Stage != "FINANCIAL_CLEARANCE" || item.UserVersion <= 0 {
		return ErrConflict
	}
	var status string
	var version int64
	err := tx.QueryRow(ctx, `SELECT u.status,u.version FROM users u JOIN account_closure_requests request ON request.user_id=u.id
WHERE request.id=$1 AND request.user_id=$2 AND request.stage=$3
 AND request.cleanup_prepared_at IS NULL AND request.identity_deleted_at IS NULL
 AND request.grants_isolated_at IS NULL AND request.token_replays_cleared_at IS NULL
 AND request.subject_lifecycle_version IS NULL AND request.completed_at IS NULL AND u.anonymized_at IS NULL
FOR UPDATE OF u`, item.ID, item.UserID, item.Stage).Scan(&status, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if (status != "ACTIVE" && status != "CLOSING") || status != item.UserStatus || version != item.UserVersion {
		return ErrConflict
	}
	// A worker may have waited for the user lock past its lease deadline.
	return requireAccountClosureClaimTx(ctx, tx, item)
}

func accountClosureNeedsSelfService(blockers []AccountClosureBlocker) bool {
	for _, blocker := range blockers {
		if blocker.Domain != "BILLING" {
			continue
		}
		switch {
		case blocker.Code == "PERSONAL_BALANCE_NONZERO" && blocker.UserAction == "CLEAR_PERSONAL_BALANCE",
			blocker.Code == "RECEIVABLE_OUTSTANDING" && blocker.UserAction == "SETTLE_RECEIVABLE",
			blocker.Code == "PAYMENT_PROCESSING" && blocker.UserAction == "FINISH_PAYMENT_ORDER":
			return true
		}
	}
	return false
}
