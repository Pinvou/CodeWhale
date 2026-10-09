package token

import (
	"context"

	"encoding/json"
	"errors"

	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (r *PostgresRepository) MarkReview(ctx context.Context, usageID uuid.UUID, reason string, snapshot map[string]any) error {
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	reason = strings.TrimSpace(reason)
	status := "REVIEW"
	if reason == "settlement_pending" || reason == "local_commit_record_pending" {
		status = "SETTLEMENT_PENDING"
	}
	now := r.now()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	settlementStatus := "REVIEW"
	if status == "SETTLEMENT_PENDING" || reason == "upstream_transport_unknown" || reason == "upstream_error_unknown_usage" || reason == "stream_usage_missing" || reason == "stream_terminal_missing" || reason == "usage_missing" {
		settlementStatus = "USAGE_PENDING"
	}
	command, err := tx.Exec(ctx, `UPDATE usage_requests SET status=$2,settlement_status=$6,error_class=$3,usage_snapshot=$4,reconciliation_next_at=$5,reconciliation_lease_until=NULL,reconciliation_last_error=NULL,reconciliation_stopped_at=NULL,updated_at=$5 WHERE id=$1 AND status NOT IN ('COMMITTED','RELEASED')`, usageID, status, reason, encoded, now, settlementStatus)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return r.ensureUsageState(ctx, usageID, "COMMITTED", "RELEASED")
	}
	executionStatus := "REVIEW"
	if status == "SETTLEMENT_PENDING" {
		executionStatus = "SETTLEMENT_PENDING"
	}
	if err := transitionExecutionTx(ctx, tx, usageID, []string{"RECEIVED", "HOLD_RESERVING", "HOLD_RESERVED", "DISPATCH_INTENT_PERSISTED", "UPSTREAM_DISPATCHING", "STREAMING", "SETTLEMENT_PENDING", "REVIEW"}, executionStatus, reason, now); err != nil {
		return err
	}
	if err := appendUsageConvergenceEvidenceTx(ctx, tx, usageID, "UNCERTAINTY", "GATEWAY", "REVIEW", reason, map[string]any{"reason": reason, "sanitized_observation": snapshot, "automatic_high_risk_retry": false}, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) ensureUsageState(ctx context.Context, usageID uuid.UUID, allowed ...string) error {
	var status string
	if err := r.pool.QueryRow(ctx, `SELECT status FROM usage_requests WHERE id=$1`, usageID).Scan(&status); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if !slices.Contains(allowed, status) {
		return ErrForbidden
	}
	return nil
}
