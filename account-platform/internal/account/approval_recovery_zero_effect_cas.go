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

var ErrApprovalRecoveryZeroEffectConflict = errors.New("approval recovery zero-effect invalidation conflicted")

// approvalRecoveryZeroEffectCode is the closed invalidation reason for the one
// recovery transition that is allowed to close an execution with no effect.
const approvalRecoveryZeroEffectCode = "RECOVERY_ZERO_EFFECT_PROVEN"

// InvalidateOriginalApprovalZeroEffect converges an original execution that both
// owner authorities proved had no effect: the caller has already re-read the
// effect authority (ABSENT under a live lease) and the result authority (no
// positive result), and holds the live query lease. It reuses the existing
// INVALIDATED transition - no new state machine - and rechecks every frozen
// binding, the lease and the execution window inside one Serializable Account
// transaction before the CAS. It never calls a domain executor, never creates a
// second execution and never authorizes a retry.
func (r *PostgresRepository) InvalidateOriginalApprovalZeroEffect(ctx context.Context, candidate ApprovalRecoveryCandidate, lease RecoveryQueryLease) error {
	if r == nil || r.pool == nil || !validRecoveryInspectionCandidate(candidate) || candidate.OriginalKey == "" ||
		candidate.RequestVersion < 1 || candidate.ApprovalEpoch < 1 || candidate.ExecutionDeadline.IsZero() ||
		!candidate.WindowElapsed || !candidate.Investigation.QueryOnly || candidate.Investigation.Retryable ||
		lease.RequestID != candidate.RequestID || lease.Token == uuid.Nil || lease.Version < 1 {
		return ErrApprovalRecoveryZeroEffectConflict
	}
	switch candidate.Investigation.Class {
	case approvalconsumer.FailurePermanent, approvalconsumer.FailureFactOrPolicyChanged:
	default:
		// An UNKNOWN, window-ended or transient class can never invalidate: only a
		// definite pre-effect failure is eligible.
		return ErrApprovalRecoveryZeroEffectConflict
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
		return ErrApprovalRecoveryZeroEffectConflict
	}
	// The window is rechecked from the row under lock, never from the caller.
	now := time.Now().UTC()
	if item.ExecutionDeadline.After(now) {
		return ErrApprovalRecoveryZeroEffectConflict
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
		return ErrApprovalRecoveryZeroEffectConflict
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
		leaseEventID != candidate.OriginalEventID || leaseEpoch != candidate.ApprovalEpoch || !leaseUntil.After(now) {
		return ErrApprovalRecoveryZeroEffectConflict
	}
	delivery, err := readRecoveryDeliveryState(ctx, tx, candidate)
	if err != nil {
		return err
	}
	if delivery.ExecutionStatus == nil || *delivery.ExecutionStatus != executionStatus || delivery.DomainAttemptCount > 1 ||
		(candidate.Workload == approvalconsumer.BillingWorkload && delivery.DomainAttemptCount != 1) {
		return ErrApprovalRecoveryZeroEffectConflict
	}
	evidence, err := json.Marshal(map[string]any{
		"original_event_id": candidate.OriginalEventID.String(), "approval_epoch": candidate.ApprovalEpoch,
		"durable": candidate.Durable, "effect_observation": "ABSENT", "result_observation": "UNCONFIRMED",
		"failure_class": string(candidate.Investigation.Class), "lease_version": lease.Version,
		"execution_deadline": candidate.ExecutionDeadline.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return ErrApprovalRecoveryZeroEffectConflict
	}
	if err := updateExactlyOneRecoveryRow(ctx, tx, `UPDATE approval_requests SET status='INVALIDATED',invalidation_code=$2,
invalidation_details=$3,version=version+1,updated_at=$4 WHERE id=$1 AND status IN ('EXECUTING','EXECUTION_FAILED')`,
		candidate.RequestID, approvalRecoveryZeroEffectCode, evidence, now); err != nil {
		return err
	}
	if err := appendApprovalStateEvent(ctx, tx, candidate.RequestID, item.Status, "INVALIDATED", "SERVICE", "account-approval-recovery",
		approvalRecoveryZeroEffectCode, map[string]any{
			"original_event_id": candidate.OriginalEventID, "lease_version": lease.Version,
			"failure_class": string(candidate.Investigation.Class),
		}, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
