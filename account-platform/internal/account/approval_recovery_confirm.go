package account

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrApprovalRecoveryConfirmConflict = errors.New("approval recovery original result confirmation conflicted")

// ConfirmOriginalApprovalSuccess converges an already-persisted positive
// domain result. The caller must independently read that result from the
// domain authority; this method never calls a domain executor or creates a
// second Account execution. A query lease and every frozen original binding
// are rechecked under the Account transaction before any status change.
func (r *PostgresRepository) ConfirmOriginalApprovalSuccess(ctx context.Context, candidate ApprovalRecoveryCandidate, lease RecoveryQueryLease, result RecoveryResultObservation) error {
	if r == nil || r.pool == nil || !validRecoveryInspectionCandidate(candidate) || candidate.OriginalKey == "" ||
		candidate.RequestVersion < 1 || candidate.ApprovalEpoch < 1 || candidate.ExecutionDeadline.IsZero() ||
		lease.RequestID != candidate.RequestID || lease.Token == uuid.Nil || lease.Version < 1 ||
		result.State != "SUCCEEDED" || result.ObservedAt.IsZero() || result.ObservedAt.After(time.Now().UTC().Add(30*time.Second)) {
		return ErrApprovalRecoveryConfirmConflict
	}
	var domainResult, ledgerReferences json.RawMessage
	if candidate.Workload == approvalconsumer.BillingWorkload {
		if result.CommandReference != nil || !validBillingRecoveryPayload(candidate.CommandType, result.DomainResult, result.LedgerReferences) {
			return ErrApprovalRecoveryConfirmConflict
		}
		domainResult, ledgerReferences = result.DomainResult, result.LedgerReferences
	} else if candidate.Workload == approvalconsumer.TokenWorkload {
		if result.CommandReference == nil || *result.CommandReference == uuid.Nil || len(result.DomainResult) != 0 || len(result.LedgerReferences) != 0 {
			return ErrApprovalRecoveryConfirmConflict
		}
		var err error
		domainResult, err = json.Marshal(map[string]any{"command_reference": result.CommandReference, "status": "SUCCEEDED"})
		if err != nil {
			return ErrApprovalRecoveryConfirmConflict
		}
		ledgerReferences = json.RawMessage(`{}`)
	} else {
		return ErrApprovalRecoveryConfirmConflict
	}
	if _, err := canonicalSafeJSONObject(domainResult); err != nil {
		return ErrApprovalRecoveryConfirmConflict
	}
	if _, err := canonicalSafeJSONObject(ledgerReferences); err != nil {
		return ErrApprovalRecoveryConfirmConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	item, err := approvalRequestByIDForUpdate(ctx, tx, candidate.RequestID)
	if err != nil {
		return err
	}
	if item.Version != candidate.RequestVersion || item.CommandType != candidate.CommandType || item.PolicyVersion != candidate.PolicyVersion ||
		item.CommandFingerprint != candidate.CommandFingerprint || item.Status != candidate.RequestStatus ||
		item.ExecutionDeadline == nil || !item.ExecutionDeadline.Equal(candidate.ExecutionDeadline) ||
		(item.Status != "EXECUTING" && item.Status != "EXECUTION_FAILED") {
		return ErrApprovalRecoveryConfirmConflict
	}
	var executionID uuid.UUID
	var executionStatus, fingerprint, originalKey, executor, jobRunID, eventDurable, serialization string
	var eventID uuid.UUID
	var approvalEpoch int64
	err = tx.QueryRow(ctx, `SELECT id,status,encode(command_fingerprint,'hex'),execution_idempotency_key,executor_identity,job_run_id,
authorization_event_id,authorization_durable,authorization_approval_epoch,authorization_serialization_version
FROM approval_executions WHERE approval_request_id=$1 AND command_type=$2 FOR UPDATE`, candidate.RequestID, candidate.CommandType).
		Scan(&executionID, &executionStatus, &fingerprint, &originalKey, &executor, &jobRunID, &eventID, &eventDurable, &approvalEpoch, &serialization)
	if err != nil {
		return err
	}
	if executionID != candidate.ExecutionID || executionStatus != candidate.ExecutionStatus || fingerprint != candidate.CommandFingerprint ||
		originalKey != candidate.OriginalKey || executor != candidate.Workload || jobRunID != candidate.OriginalEventID.String() ||
		eventID != candidate.OriginalEventID || eventDurable != candidate.Durable || approvalEpoch != candidate.ApprovalEpoch ||
		serialization != "APPROVAL_RFC8785_DECIMAL_V1" ||
		(item.Status == "EXECUTING" && executionStatus != "EXECUTING") || (item.Status == "EXECUTION_FAILED" && executionStatus != "FAILED") {
		return ErrApprovalRecoveryConfirmConflict
	}
	var token, leaseEventID, leaseExecutionID uuid.UUID
	var leaseEpoch, leaseVersion int64
	var leaseUntil time.Time
	err = tx.QueryRow(ctx, `SELECT lease_token,lease_until,version,execution_id,original_event_id,approval_epoch
FROM approval_recovery_query_leases WHERE approval_request_id=$1 FOR UPDATE`, candidate.RequestID).
		Scan(&token, &leaseUntil, &leaseVersion, &leaseExecutionID, &leaseEventID, &leaseEpoch)
	if err != nil {
		return err
	}
	if token != lease.Token || leaseVersion != lease.Version || leaseExecutionID != candidate.ExecutionID ||
		leaseEventID != candidate.OriginalEventID || leaseEpoch != candidate.ApprovalEpoch || !leaseUntil.After(time.Now().UTC()) {
		return ErrApprovalRecoveryConfirmConflict
	}
	delivery, err := readRecoveryDeliveryState(ctx, tx, candidate)
	if err != nil {
		return err
	}
	if delivery.ExecutionStatus == nil || *delivery.ExecutionStatus != executionStatus || delivery.DomainAttemptCount > 1 ||
		(candidate.Workload == approvalconsumer.BillingWorkload && delivery.DomainAttemptCount != 1) {
		return ErrApprovalRecoveryConfirmConflict
	}
	now := time.Now().UTC()
	previous := item.Status
	if previous == "EXECUTION_FAILED" {
		if err := updateExactlyOneRecoveryRow(ctx, tx, `UPDATE approval_executions SET status='EXECUTING',error_class=NULL,completed_at=NULL WHERE id=$1 AND status='FAILED'`, executionID); err != nil {
			return err
		}
		if err := updateExactlyOneRecoveryRow(ctx, tx, `UPDATE approval_requests SET status='EXECUTING',version=version+1,updated_at=$2 WHERE id=$1 AND status='EXECUTION_FAILED'`, candidate.RequestID, now); err != nil {
			return err
		}
		if err := appendApprovalStateEvent(ctx, tx, candidate.RequestID, "EXECUTION_FAILED", "EXECUTING", "SERVICE", "account-approval-recovery", "ORIGINAL_RESULT_CONFIRMED", map[string]any{"original_event_id": eventID, "lease_version": lease.Version}, now); err != nil {
			return err
		}
	}
	if err := updateExactlyOneRecoveryRow(ctx, tx, `UPDATE approval_executions SET status='EXECUTED',domain_result=$2,ledger_references=$3,error_class=NULL,completed_at=$4
WHERE id=$1 AND status='EXECUTING'`, executionID, domainResult, ledgerReferences, now); err != nil {
		return err
	}
	if err := updateExactlyOneRecoveryRow(ctx, tx, `UPDATE approval_requests SET status='EXECUTED',version=version+1,updated_at=$2 WHERE id=$1 AND status='EXECUTING'`, candidate.RequestID, now); err != nil {
		return err
	}
	if err := appendApprovalStateEvent(ctx, tx, candidate.RequestID, "EXECUTING", "EXECUTED", "SERVICE", "account-approval-recovery", "ORIGINAL_RESULT_CONFIRMED", map[string]any{"original_event_id": eventID, "lease_version": lease.Version}, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func updateExactlyOneRecoveryRow(ctx context.Context, tx pgx.Tx, sql string, args ...any) error {
	command, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrApprovalRecoveryConfirmConflict
	}
	return nil
}
