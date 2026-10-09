package token

import (
	"context"

	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func cancellationSnapshot(usage UsageRequest) CancellationResult {
	return CancellationResult{
		UsageRequestID: usage.ID, ExecutionStatus: usage.ExecutionStatus,
		CancelStatus: usage.CancelStatus, DeliveryStatus: usage.DeliveryStatus,
		SettlementStatus: usage.SettlementStatus, CancelRequestedAt: usage.CancelRequestedAt,
		ProviderTerminalAt: usage.ProviderTerminalAt, FinalUsageObservedAt: usage.FinalUsageObservedAt,
	}
}

func (r *PostgresRepository) ConfirmPreDispatchCancellation(ctx context.Context, usageID uuid.UUID) (CancellationResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return CancellationResult{}, err
	}
	defer tx.Rollback(ctx)
	var usage UsageRequest
	if err := scanUsage(tx.QueryRow(ctx, `SELECT `+usageSelectColumns+` FROM usage_requests WHERE id=$1 FOR UPDATE`, usageID), &usage); errors.Is(err, pgx.ErrNoRows) {
		return CancellationResult{}, ErrNotFound
	} else if err != nil {
		return CancellationResult{}, err
	}
	if usage.CancelStatus == "LOCAL_CONFIRMED" || usage.ExecutionStatus == "CANCELLED_BEFORE_DISPATCH" {
		return cancellationSnapshot(usage), tx.Commit(ctx)
	}
	if usage.CancelStatus != "CANCEL_REQUESTED" || (usage.ExecutionStatus != "RECEIVED" && usage.ExecutionStatus != "HOLD_RESERVING" && usage.ExecutionStatus != "HOLD_RESERVED") {
		return CancellationResult{}, ErrForbidden
	}
	if usage.CanonicalVersion == 0 && usage.HoldID == nil {
		return CancellationResult{}, ErrForbidden
	}
	now := r.now()
	if err := transitionExecutionTx(ctx, tx, usageID, []string{"RECEIVED", "HOLD_RESERVING", "HOLD_RESERVED"}, "CANCELLED_BEFORE_DISPATCH", "user cancellation confirmed before dispatch intent", now); err != nil {
		return CancellationResult{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE usage_requests SET status='RELEASED',settlement_status='RELEASED',cancel_status='LOCAL_CONFIRMED',terminal_at=$2,updated_at=$2,reconciliation_next_at=NULL,reconciliation_lease_until=NULL,reconciliation_stopped_at=NULL WHERE id=$1`, usageID, now); err != nil {
		return CancellationResult{}, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM usage_dispatch_preparations WHERE usage_request_id=$1`, usageID); err != nil {
		return CancellationResult{}, err
	}
	if err := addReplayTombstone(ctx, tx, usageID, "CANCELLED_BEFORE_DISPATCH", now); err != nil {
		return CancellationResult{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO usage_cancellation_evidence(id,usage_request_id,evidence_type,adapter_version,sanitized_evidence,occurred_at) VALUES($1,$2,'LOCAL_CONFIRMED','local',jsonb_build_object('provider_called',false,'hold_released',$3::boolean),$4)`, uuid.Must(uuid.NewV7()), usageID, usage.HoldID != nil, now); err != nil {
		return CancellationResult{}, err
	}
	usage.Status, usage.ExecutionStatus, usage.CancelStatus, usage.SettlementStatus, usage.UpdatedAt = "RELEASED", "CANCELLED_BEFORE_DISPATCH", "LOCAL_CONFIRMED", "RELEASED", now
	return cancellationSnapshot(usage), tx.Commit(ctx)
}
