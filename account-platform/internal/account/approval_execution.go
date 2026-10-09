package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (r *PostgresRepository) BeginApprovalExecution(ctx context.Context, requestID uuid.UUID, executorIdentity, jobRunID string, observedVersions, observedFinancialSnapshot json.RawMessage) (ApprovalExecution, error) {
	return r.beginApprovalExecution(ctx, requestID, executorIdentity, jobRunID, observedVersions, observedFinancialSnapshot, nil)
}

func (r *PostgresRepository) BeginApprovalExecutionWithAuthorization(ctx context.Context, requestID uuid.UUID, executorIdentity, jobRunID string, observedVersions, observedFinancialSnapshot json.RawMessage, binding ApprovalAuthorizationBinding) (ApprovalExecution, error) {
	if executorIdentity != strings.TrimSpace(executorIdentity) || binding.ApprovalRequestID != requestID || binding.Workload != executorIdentity {
		return ApprovalExecution{}, ErrApprovalConsumerBindingInvalid
	}
	return r.beginApprovalExecution(ctx, requestID, executorIdentity, jobRunID, observedVersions, observedFinancialSnapshot, &binding)
}

func (r *PostgresRepository) beginApprovalExecution(ctx context.Context, requestID uuid.UUID, executorIdentity, jobRunID string, observedVersions, observedFinancialSnapshot json.RawMessage, binding *ApprovalAuthorizationBinding) (ApprovalExecution, error) {
	executorIdentity, jobRunID = strings.TrimSpace(executorIdentity), strings.TrimSpace(jobRunID)
	if executorIdentity == "" || jobRunID == "" {
		return ApprovalExecution{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ApprovalExecution{}, err
	}
	defer tx.Rollback(ctx)
	if binding != nil {
		if err := validateApprovalAuthorizationBegin(ctx, tx, *binding); err != nil {
			return ApprovalExecution{}, err
		}
	} else if err := validateLegacyApprovalAuthorizationBegin(ctx, tx, requestID, executorIdentity); err != nil {
		return ApprovalExecution{}, err
	}
	item, err := approvalRequestByIDForUpdate(ctx, tx, requestID)
	if err != nil {
		return ApprovalExecution{}, err
	}
	policy, err := approvalPolicyForItem(item)
	if err != nil || policy.ExecutorIdentity != executorIdentity {
		return ApprovalExecution{}, ErrForbidden
	}
	if err := assertStoredApprovalPolicy(ctx, tx, policy); err != nil {
		if errors.Is(err, approvalpolicy.ErrPolicyNotRegistered) {
			return invalidateExecutionAuthorization(ctx, tx, item, executorIdentity, "POLICY_VERSION_CHANGED", r.now())
		}
		return ApprovalExecution{}, err
	}
	if item.Status == "EXECUTING" || item.Status == "EXECUTED" {
		var storedExecutor, idempotencyKey, executionStatus, decisionReason string
		var approverID uuid.UUID
		var frozenCritical, frozenFinancial json.RawMessage
		if binding == nil {
			if err := tx.QueryRow(ctx, `SELECT e.executor_identity,e.execution_idempotency_key,e.status,d.approver_user_id,d.reason,e.critical_resource_versions,e.financial_snapshot FROM approval_executions e JOIN approval_decisions d ON d.approval_request_id=e.approval_request_id AND d.decision='APPROVED' WHERE e.approval_request_id=$1 AND e.command_type=$2`, requestID, item.CommandType).Scan(&storedExecutor, &idempotencyKey, &executionStatus, &approverID, &decisionReason, &frozenCritical, &frozenFinancial); err != nil {
				return ApprovalExecution{}, err
			}
			if storedExecutor != executorIdentity {
				return ApprovalExecution{}, ErrForbidden
			}
			execution := ApprovalExecution{ApprovalRequestID: requestID, CommandType: item.CommandType, PolicyVersion: item.PolicyVersion, CommandFingerprint: item.CommandFingerprint, NormalizedCommand: item.NormalizedCommand, TargetResourceType: item.TargetResourceType, TargetResourceID: item.TargetResourceID, CriticalResourceVersions: frozenCritical, FinancialSnapshot: frozenFinancial, RequesterUserID: item.RequesterUserID, RequesterAuthorizationVersion: item.RequesterAuthorizationVersion, RequesterReason: item.Reason, ApproverUserID: approverID, DecisionReason: decisionReason, ExecutionIdempotencyKey: idempotencyKey, ExecutorIdentity: executorIdentity, Status: executionStatus}
			return execution, tx.Commit(ctx)
		}
		var storedBinding approvalExecutionAuthorizationBinding
		if err := tx.QueryRow(ctx, `SELECT e.executor_identity,e.execution_idempotency_key,e.status,d.approver_user_id,d.reason,e.critical_resource_versions,e.financial_snapshot,
e.authorization_event_id,e.authorization_durable,e.authorization_approved_request_version,e.authorization_event_digest,e.authorization_digest_kind,
e.authorization_serialization_version,e.authorization_provenance,e.authorization_approval_epoch
FROM approval_executions e JOIN approval_decisions d ON d.approval_request_id=e.approval_request_id AND d.decision='APPROVED'
WHERE e.approval_request_id=$1 AND e.command_type=$2`, requestID, item.CommandType).Scan(&storedExecutor, &idempotencyKey, &executionStatus, &approverID, &decisionReason, &frozenCritical, &frozenFinancial,
			&storedBinding.EventID, &storedBinding.Durable, &storedBinding.ApprovedRequestVersion, &storedBinding.EventDigest,
			&storedBinding.DigestKind, &storedBinding.SerializationVersion, &storedBinding.Provenance, &storedBinding.ApprovalEpoch); err != nil {
			return ApprovalExecution{}, err
		}
		if storedExecutor != executorIdentity || !approvalExecutionBindingMatches(storedBinding, binding) {
			return ApprovalExecution{}, ErrForbidden
		}
		execution := ApprovalExecution{ApprovalRequestID: requestID, CommandType: item.CommandType, PolicyVersion: item.PolicyVersion, CommandFingerprint: item.CommandFingerprint, NormalizedCommand: item.NormalizedCommand, TargetResourceType: item.TargetResourceType, TargetResourceID: item.TargetResourceID, CriticalResourceVersions: frozenCritical, FinancialSnapshot: frozenFinancial, RequesterUserID: item.RequesterUserID, RequesterAuthorizationVersion: item.RequesterAuthorizationVersion, RequesterReason: item.Reason, ApproverUserID: approverID, DecisionReason: decisionReason, ExecutionIdempotencyKey: idempotencyKey, ExecutorIdentity: executorIdentity, Status: executionStatus}
		applyApprovalExecutionBinding(&execution, storedBinding)
		return execution, tx.Commit(ctx)
	}
	if err := r.requireApprovalAdvancement(ctx, policy); err != nil {
		return ApprovalExecution{}, err
	}
	now := r.now()
	if item.ExecutionDeadline == nil || !item.ExecutionDeadline.After(now) {
		_, _ = tx.Exec(ctx, `UPDATE approval_requests SET status='INVALIDATED',invalidation_code='EXECUTION_WINDOW_EXPIRED',invalidation_details='{}',version=version+1,updated_at=$2 WHERE id=$1 AND status IN ('APPROVED','EXECUTION_FAILED')`, requestID, now)
		_ = appendApprovalStateEvent(ctx, tx, requestID, item.Status, "INVALIDATED", "SERVICE", executorIdentity, "EXECUTION_WINDOW_EXPIRED", map[string]any{}, now)
		if err := tx.Commit(ctx); err != nil {
			return ApprovalExecution{}, err
		}
		return ApprovalExecution{}, ErrApprovalInvalidated
	}
	if item.Status != "APPROVED" && item.Status != "EXECUTION_FAILED" {
		return ApprovalExecution{}, ErrConflict
	}
	financialSnapshotChanged := approvalSnapshotCommand(item.CommandType) && !sameApprovalFinancialFacts(item.CommandType, observedFinancialSnapshot, item.FinancialSnapshot)
	if !sameJSONObject(observedVersions, item.CriticalResourceVersions) || financialSnapshotChanged {
		_, _ = tx.Exec(ctx, `UPDATE approval_requests SET status='INVALIDATED',invalidation_code='RESOURCE_VERSION_CHANGED',invalidation_details=$2,version=version+1,updated_at=$3 WHERE id=$1 AND status IN ('APPROVED','EXECUTION_FAILED')`, requestID, jsonOrDefault(observedVersions, `{}`), now)
		_ = appendApprovalStateEvent(ctx, tx, requestID, item.Status, "INVALIDATED", "SERVICE", executorIdentity, "RESOURCE_VERSION_CHANGED", map[string]any{}, now)
		if err := tx.Commit(ctx); err != nil {
			return ApprovalExecution{}, err
		}
		return ApprovalExecution{}, ErrApprovalInvalidated
	}
	requesterBinding, requesterVersion, _, err := strongPlatformAuthorization(ctx, tx, item.RequesterUserID, policy.RequesterPermission)
	if err != nil && !errors.Is(err, ErrForbidden) {
		return invalidateExecutionAuthorization(ctx, tx, item, executorIdentity, "AUTHORIZATION_UNAVAILABLE", now)
	}
	if err != nil || requesterBinding != item.RequesterBindingID || requesterVersion != item.RequesterAuthorizationVersion {
		return invalidateExecutionAuthorization(ctx, tx, item, executorIdentity, "REQUESTER_AUTH_CHANGED", now)
	}
	var approverID, approverBinding uuid.UUID
	var approverVersion int64
	var decisionReason string
	if err := tx.QueryRow(ctx, `SELECT approver_user_id,approver_binding_id,approver_authorization_version,reason FROM approval_decisions WHERE approval_request_id=$1 AND decision='APPROVED'`, requestID).Scan(&approverID, &approverBinding, &approverVersion, &decisionReason); err != nil {
		return ApprovalExecution{}, ErrApprovalInvalidated
	}
	currentApproverBinding, currentApproverVersion, _, err := strongPlatformAuthorization(ctx, tx, approverID, policy.ApproverPermission)
	if err != nil && !errors.Is(err, ErrForbidden) {
		return invalidateExecutionAuthorization(ctx, tx, item, executorIdentity, "AUTHORIZATION_UNAVAILABLE", now)
	}
	if err != nil || currentApproverBinding != approverBinding || currentApproverVersion != approverVersion {
		return invalidateExecutionAuthorization(ctx, tx, item, executorIdentity, "APPROVER_AUTH_CHANGED", now)
	}
	idempotencyKey := fmt.Sprintf("approval:%s:%s:%s", requestID, item.CommandType, item.CommandFingerprint)
	executionID := uuid.Must(uuid.NewV7())
	authorizationSnapshot, _ := json.Marshal(map[string]any{"requester_user_id": item.RequesterUserID, "requester_binding_id": requesterBinding, "requester_authorization_version": requesterVersion, "approver_user_id": approverID, "approver_binding_id": approverBinding, "approver_authorization_version": approverVersion, "execution_started_at": now})
	var storedExecutionID uuid.UUID
	authorization := approvalExecutionBindingArguments(binding)
	if binding == nil {
		err = tx.QueryRow(ctx, `INSERT INTO approval_executions(id,approval_request_id,command_type,command_fingerprint,execution_idempotency_key,executor_identity,job_run_id,status,authorization_snapshot,critical_resource_versions,financial_snapshot,started_at) VALUES($1,$2,$3,decode($4,'hex'),$5,$6,$7,'EXECUTING',$8,$9,$10,$11) ON CONFLICT (approval_request_id,command_type) DO UPDATE SET status=CASE WHEN approval_executions.status='FAILED' THEN 'EXECUTING' ELSE approval_executions.status END,job_run_id=CASE WHEN approval_executions.status='FAILED' THEN EXCLUDED.job_run_id ELSE approval_executions.job_run_id END,started_at=CASE WHEN approval_executions.status='FAILED' THEN EXCLUDED.started_at ELSE approval_executions.started_at END WHERE approval_executions.command_fingerprint=EXCLUDED.command_fingerprint AND approval_executions.executor_identity=EXCLUDED.executor_identity AND approval_executions.critical_resource_versions=EXCLUDED.critical_resource_versions AND approval_executions.financial_snapshot=EXCLUDED.financial_snapshot RETURNING id`, executionID, requestID, item.CommandType, item.CommandFingerprint, idempotencyKey, executorIdentity, jobRunID, authorizationSnapshot, item.CriticalResourceVersions, item.FinancialSnapshot, now).Scan(&storedExecutionID)
	} else {
		err = tx.QueryRow(ctx, `INSERT INTO approval_executions(id,approval_request_id,command_type,command_fingerprint,execution_idempotency_key,executor_identity,job_run_id,status,authorization_snapshot,critical_resource_versions,financial_snapshot,started_at,
authorization_event_id,authorization_durable,authorization_approved_request_version,authorization_event_digest,authorization_digest_kind,authorization_serialization_version,authorization_provenance,authorization_approval_epoch)
VALUES($1,$2,$3,decode($4,'hex'),$5,$6,$7,'EXECUTING',$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
ON CONFLICT (approval_request_id,command_type) DO UPDATE SET
status=CASE WHEN approval_executions.status='FAILED' THEN 'EXECUTING' ELSE approval_executions.status END,
job_run_id=CASE WHEN approval_executions.status='FAILED' THEN EXCLUDED.job_run_id ELSE approval_executions.job_run_id END,
started_at=CASE WHEN approval_executions.status='FAILED' THEN EXCLUDED.started_at ELSE approval_executions.started_at END
WHERE approval_executions.command_fingerprint=EXCLUDED.command_fingerprint
  AND approval_executions.executor_identity=EXCLUDED.executor_identity
  AND approval_executions.critical_resource_versions=EXCLUDED.critical_resource_versions
  AND approval_executions.financial_snapshot=EXCLUDED.financial_snapshot
  AND approval_executions.authorization_event_id IS NOT DISTINCT FROM EXCLUDED.authorization_event_id
  AND approval_executions.authorization_durable IS NOT DISTINCT FROM EXCLUDED.authorization_durable
  AND approval_executions.authorization_approved_request_version IS NOT DISTINCT FROM EXCLUDED.authorization_approved_request_version
  AND approval_executions.authorization_event_digest IS NOT DISTINCT FROM EXCLUDED.authorization_event_digest
  AND approval_executions.authorization_digest_kind IS NOT DISTINCT FROM EXCLUDED.authorization_digest_kind
  AND approval_executions.authorization_serialization_version IS NOT DISTINCT FROM EXCLUDED.authorization_serialization_version
  AND approval_executions.authorization_provenance IS NOT DISTINCT FROM EXCLUDED.authorization_provenance
  AND approval_executions.authorization_approval_epoch IS NOT DISTINCT FROM EXCLUDED.authorization_approval_epoch
RETURNING id`, executionID, requestID, item.CommandType, item.CommandFingerprint, idempotencyKey, executorIdentity, jobRunID,
			authorizationSnapshot, item.CriticalResourceVersions, item.FinancialSnapshot, now,
			authorization.EventID, authorization.Durable, authorization.ApprovedRequestVersion, authorization.EventDigest,
			authorization.DigestKind, authorization.SerializationVersion, authorization.Provenance, authorization.ApprovalEpoch).Scan(&storedExecutionID)
	}
	if err != nil {
		return ApprovalExecution{}, ErrConflict
	}
	if item.Status == "APPROVED" || item.Status == "EXECUTION_FAILED" {
		if _, err := tx.Exec(ctx, `UPDATE approval_requests SET status='EXECUTING',version=version+1,updated_at=$2 WHERE id=$1`, requestID, now); err != nil {
			return ApprovalExecution{}, err
		}
		if err := appendApprovalStateEvent(ctx, tx, requestID, item.Status, "EXECUTING", "SERVICE", executorIdentity, "EXECUTION_STARTED", map[string]any{"job_run_id": jobRunID, "idempotency_key": idempotencyKey}, now); err != nil {
			return ApprovalExecution{}, err
		}
	}
	// RETURNING the durable immutable ID distinguishes a true INSERT from a
	// FAILED-row conflict update. RowsAffected=1 cannot prove first dispatch.
	execution := ApprovalExecution{ApprovalRequestID: requestID, CommandType: item.CommandType, PolicyVersion: item.PolicyVersion, CommandFingerprint: item.CommandFingerprint, NormalizedCommand: item.NormalizedCommand, TargetResourceType: item.TargetResourceType, TargetResourceID: item.TargetResourceID, CriticalResourceVersions: item.CriticalResourceVersions, FinancialSnapshot: item.FinancialSnapshot, RequesterUserID: item.RequesterUserID, RequesterAuthorizationVersion: item.RequesterAuthorizationVersion, RequesterReason: item.Reason, FirstClaim: storedExecutionID == executionID, ApproverUserID: approverID, DecisionReason: decisionReason, ExecutionIdempotencyKey: idempotencyKey, ExecutorIdentity: executorIdentity, Status: "EXECUTING"}
	applyApprovalExecutionBinding(&execution, authorization)
	return execution, tx.Commit(ctx)
}

type approvalExecutionAuthorizationBinding struct {
	EventID                *uuid.UUID
	Durable                *string
	ApprovedRequestVersion *int64
	EventDigest            *string
	DigestKind             *string
	SerializationVersion   *string
	Provenance             *string
	ApprovalEpoch          *int64
}

func approvalExecutionBindingArguments(binding *ApprovalAuthorizationBinding) approvalExecutionAuthorizationBinding {
	if binding == nil {
		return approvalExecutionAuthorizationBinding{}
	}
	return approvalExecutionAuthorizationBinding{
		EventID: &binding.EventID, Durable: &binding.Durable, ApprovedRequestVersion: &binding.ApprovedRequestVersion,
		EventDigest: &binding.EventDigest, DigestKind: &binding.DigestKind, SerializationVersion: &binding.SerializationVersion,
		Provenance: &binding.Provenance, ApprovalEpoch: &binding.ApprovalEpoch,
	}
}

func approvalExecutionBindingMatches(stored approvalExecutionAuthorizationBinding, expected *ApprovalAuthorizationBinding) bool {
	if expected == nil {
		return stored.EventID == nil && stored.Durable == nil && stored.ApprovedRequestVersion == nil && stored.EventDigest == nil &&
			stored.DigestKind == nil && stored.SerializationVersion == nil && stored.Provenance == nil && stored.ApprovalEpoch == nil
	}
	return stored.EventID != nil && *stored.EventID == expected.EventID && stored.Durable != nil && *stored.Durable == expected.Durable &&
		stored.ApprovedRequestVersion != nil && *stored.ApprovedRequestVersion == expected.ApprovedRequestVersion &&
		stored.EventDigest != nil && *stored.EventDigest == expected.EventDigest && stored.DigestKind != nil && *stored.DigestKind == expected.DigestKind &&
		stored.SerializationVersion != nil && *stored.SerializationVersion == expected.SerializationVersion &&
		stored.Provenance != nil && *stored.Provenance == expected.Provenance && stored.ApprovalEpoch != nil && *stored.ApprovalEpoch == expected.ApprovalEpoch
}

func applyApprovalExecutionBinding(execution *ApprovalExecution, binding approvalExecutionAuthorizationBinding) {
	if execution == nil || binding.EventID == nil {
		return
	}
	execution.AuthorizationEventID = binding.EventID
	if binding.Durable != nil {
		execution.AuthorizationDurable = *binding.Durable
	}
	if binding.ApprovedRequestVersion != nil {
		execution.AuthorizationApprovedVersion = *binding.ApprovedRequestVersion
	}
	if binding.EventDigest != nil {
		execution.AuthorizationEventDigest = *binding.EventDigest
	}
	if binding.DigestKind != nil {
		execution.AuthorizationDigestKind = *binding.DigestKind
	}
	if binding.SerializationVersion != nil {
		execution.AuthorizationSerialization = *binding.SerializationVersion
	}
	if binding.Provenance != nil {
		execution.AuthorizationProvenance = *binding.Provenance
	}
	if binding.ApprovalEpoch != nil {
		execution.AuthorizationApprovalEpoch = *binding.ApprovalEpoch
	}
}

func invalidateExecutionAuthorization(ctx context.Context, tx pgx.Tx, item ApprovalRequest, executorIdentity, code string, now time.Time) (ApprovalExecution, error) {
	if _, err := tx.Exec(ctx, `UPDATE approval_requests SET status='INVALIDATED',invalidation_code=$2,invalidation_details='{}',version=version+1,updated_at=$3 WHERE id=$1`, item.ID, code, now); err != nil {
		return ApprovalExecution{}, err
	}
	if err := appendApprovalStateEvent(ctx, tx, item.ID, item.Status, "INVALIDATED", "SERVICE", executorIdentity, code, map[string]any{}, now); err != nil {
		return ApprovalExecution{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ApprovalExecution{}, err
	}
	return ApprovalExecution{}, ErrApprovalInvalidated
}

func (r *PostgresRepository) CompleteApprovalExecution(ctx context.Context, requestID uuid.UUID, executorIdentity, jobRunID, result string, domainResult, ledgerReferences json.RawMessage, errorClass string) error {
	result = strings.ToUpper(strings.TrimSpace(result))
	if result != "EXECUTED" && result != "FAILED" {
		return ErrConflict
	}
	if (result == "EXECUTED" && errorClass != "") || (result == "FAILED" && !validStableExecutionErrorCode(errorClass)) {
		return ErrConflict
	}
	if _, err := canonicalSafeJSONObject(jsonOrDefault(domainResult, `{}`)); err != nil {
		return err
	}
	if _, err := canonicalSafeJSONObject(jsonOrDefault(ledgerReferences, `{}`)); err != nil {
		return err
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	item, err := approvalRequestByIDForUpdate(ctx, tx, requestID)
	if err != nil {
		return err
	}
	var executionStatus, storedExecutor, storedJobRunID string
	if err := tx.QueryRow(ctx, `SELECT status,executor_identity,job_run_id FROM approval_executions WHERE approval_request_id=$1 AND command_type=$2 FOR UPDATE`, requestID, item.CommandType).Scan(&executionStatus, &storedExecutor, &storedJobRunID); err != nil {
		return err
	}
	if storedExecutor != strings.TrimSpace(executorIdentity) || storedJobRunID != strings.TrimSpace(jobRunID) {
		return ErrForbidden
	}
	if executionStatus == "EXECUTED" && result == "EXECUTED" {
		return tx.Commit(ctx)
	}
	if executionStatus != "EXECUTING" || item.Status != "EXECUTING" {
		return ErrConflict
	}
	now := r.now()
	requestStatus := "EXECUTED"
	if result == "FAILED" {
		requestStatus = "EXECUTION_FAILED"
		if strings.TrimSpace(errorClass) == "" {
			return ErrConflict
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE approval_executions SET status=$2,domain_result=$3,ledger_references=$4,error_class=NULLIF($5,''),completed_at=$6 WHERE approval_request_id=$1 AND status='EXECUTING'`, requestID, result, jsonOrDefault(domainResult, `{}`), jsonOrDefault(ledgerReferences, `{}`), strings.TrimSpace(errorClass), now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE approval_requests SET status=$2,version=version+1,updated_at=$3 WHERE id=$1`, requestID, requestStatus, now); err != nil {
		return err
	}
	if err := appendApprovalStateEvent(ctx, tx, requestID, "EXECUTING", requestStatus, "SERVICE", executorIdentity, result, map[string]any{"job_run_id": jobRunID, "error_class": errorClass}, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func validStableExecutionErrorCode(code string) bool {
	if len(code) < 1 || len(code) > 120 || code != strings.TrimSpace(code) || code[0] < 'A' || code[0] > 'Z' {
		return false
	}
	for index := 1; index < len(code); index++ {
		if current := code[index]; (current < 'A' || current > 'Z') && (current < '0' || current > '9') && current != '_' {
			return false
		}
	}
	for _, sensitive := range []string{"TOKEN", "KEY", "SECRET", "PASSWORD", "CREDENTIAL"} {
		if strings.Contains(code, sensitive) {
			return false
		}
	}
	return true
}

func canonicalSafeJSONObject(raw json.RawMessage) ([]byte, error) {
	canonical, err := canonicalSafeJSON(raw)
	if err != nil {
		return nil, err
	}
	var object map[string]any
	if json.Unmarshal(canonical, &object) != nil {
		return nil, ErrConflict
	}
	return canonical, nil
}
