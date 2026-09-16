package account

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvaleffect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrApprovalReconciliationBlocked        = errors.New("approval reconciliation is blocked")
	ErrApprovalReconciliationControlChanged = errors.New("approval reconciliation control changed")
)

type ApprovalReconciliationCandidate struct {
	RequestID          uuid.UUID `json:"approval_request_id"`
	CommandType        string    `json:"command_type"`
	CommandFingerprint string    `json:"command_fingerprint"`
	PolicyVersion      int64     `json:"policy_version"`
	ExecutorIdentity   string    `json:"executor_identity"`
	Status             string    `json:"status"`
	Version            int64     `json:"version"`
	ExecutionDeadline  time.Time `json:"execution_deadline"`
}

type ApprovalReconciliationPlan struct {
	Control        AdminApprovalControl
	Candidates     []ApprovalReconciliationCandidate
	MoreCandidates bool
}

type ApprovalReconciliationResult string

const (
	ApprovalReconciliationInvalidated            ApprovalReconciliationResult = "INVALIDATED"
	ApprovalReconciliationExclusionSealed        ApprovalReconciliationResult = "INVALIDATED_EXCLUSION_SEALED"
	ApprovalReconciliationStale                  ApprovalReconciliationResult = "STALE"
	ApprovalReconciliationBlockedExecution       ApprovalReconciliationResult = "BLOCKED_EXECUTION"
	ApprovalReconciliationBlockedHistory         ApprovalReconciliationResult = "BLOCKED_HISTORY"
	ApprovalReconciliationBlockedEffect          ApprovalReconciliationResult = "BLOCKED_EFFECT"
	ApprovalReconciliationBlockedEffectAuthority ApprovalReconciliationResult = "BLOCKED_EFFECT_AUTHORITY"
)

type ApprovalReconciliationSummary struct {
	Mode                   AdminApprovalMode `json:"mode"`
	ApprovalEpoch          int64             `json:"approval_epoch"`
	ControlVersion         int64             `json:"control_version"`
	CandidateCount         int               `json:"candidate_count"`
	EligibleCount          int               `json:"eligible_count"`
	InvalidatedCount       int               `json:"invalidated_count"`
	SealedExclusionCount   int               `json:"sealed_exclusion_count"`
	ObservedEffectCount    int               `json:"observed_effect_count"`
	UnknownEffectCount     int               `json:"unknown_effect_count"`
	ExecutionBlockedCount  int               `json:"execution_blocked_count"`
	HistoryBlockedCount    int               `json:"history_blocked_count"`
	StaleCount             int               `json:"stale_count"`
	MoreCandidates         bool              `json:"more_candidates"`
	DryRun                 bool              `json:"dry_run"`
	CompleteWithoutBlocker bool              `json:"complete_without_blocker"`
}

type ApprovalReconciliationStore interface {
	PlanExpiredApprovalReconciliation(context.Context, int) (ApprovalReconciliationPlan, error)
	ReconcileExpiredApproval(context.Context, AdminApprovalControl, ApprovalReconciliationCandidate, approvaleffect.Observation) (ApprovalReconciliationResult, error)
}

func (r *PostgresRepository) PlanExpiredApprovalReconciliation(ctx context.Context, limit int) (ApprovalReconciliationPlan, error) {
	if limit < 1 || limit > 200 {
		return ApprovalReconciliationPlan{}, ErrApprovalReconciliationBlocked
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ApprovalReconciliationPlan{}, err
	}
	defer tx.Rollback(ctx)
	control, err := ReadAdminApprovalControl(ctx, tx)
	if err != nil {
		return ApprovalReconciliationPlan{}, err
	}
	if control.Mode != AdminApprovalModeQuiescing && control.Mode != AdminApprovalModeSafeRollback {
		return ApprovalReconciliationPlan{}, ErrApprovalReconciliationBlocked
	}
	descriptors := approvalconsumer.All()
	commands := make([]string, 0, len(descriptors))
	for _, descriptor := range descriptors {
		commands = append(commands, descriptor.CommandType)
	}
	rows, err := tx.Query(ctx, `SELECT r.id,r.command_type,encode(r.command_fingerprint,'hex'),r.policy_version,p.executor_identity,
  r.status,r.version,r.execution_deadline
FROM approval_requests r
JOIN approval_command_policies p ON p.command_type=r.command_type AND p.policy_version=r.policy_version
WHERE r.command_type=ANY($1) AND r.status IN ('APPROVED','EXECUTION_FAILED')
  AND r.execution_deadline IS NOT NULL AND r.execution_deadline<=transaction_timestamp()
ORDER BY r.execution_deadline,r.id LIMIT $2`, commands, limit+1)
	if err != nil {
		return ApprovalReconciliationPlan{}, err
	}
	defer rows.Close()
	plan := ApprovalReconciliationPlan{Control: control, Candidates: make([]ApprovalReconciliationCandidate, 0)}
	for rows.Next() {
		var candidate ApprovalReconciliationCandidate
		if err := rows.Scan(&candidate.RequestID, &candidate.CommandType, &candidate.CommandFingerprint, &candidate.PolicyVersion,
			&candidate.ExecutorIdentity, &candidate.Status, &candidate.Version, &candidate.ExecutionDeadline); err != nil {
			return ApprovalReconciliationPlan{}, err
		}
		candidate.ExecutionDeadline = candidate.ExecutionDeadline.UTC()
		if candidate.RequestID == uuid.Nil || candidate.Version < 1 || !approvaleffect.ValidFingerprint(candidate.CommandFingerprint) ||
			(candidate.Status != "APPROVED" && candidate.Status != "EXECUTION_FAILED") || !reconciliationDescriptorMatches(descriptors, candidate) {
			return ApprovalReconciliationPlan{}, ErrApprovalReconciliationBlocked
		}
		plan.Candidates = append(plan.Candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return ApprovalReconciliationPlan{}, err
	}
	if len(plan.Candidates) > limit {
		plan.Candidates = plan.Candidates[:limit]
		plan.MoreCandidates = true
	}
	if err := tx.Commit(ctx); err != nil {
		return ApprovalReconciliationPlan{}, err
	}
	return plan, nil
}

func reconciliationDescriptorMatches(descriptors []approvalconsumer.Descriptor, candidate ApprovalReconciliationCandidate) bool {
	for _, descriptor := range descriptors {
		if descriptor.CommandType == candidate.CommandType && descriptor.PolicyVersion == candidate.PolicyVersion &&
			descriptor.ExecutorIdentity == candidate.ExecutorIdentity {
			return true
		}
	}
	return false
}

type reconciliationOutbox struct {
	eventID        uuid.UUID
	eventDigest    string
	approvalEpoch  int64
	publishedAt    *time.Time
	deadLetteredAt *time.Time
	lockedBy       *string
	lockedUntil    *time.Time
}

func (r *PostgresRepository) ReconcileExpiredApproval(ctx context.Context, expectedControl AdminApprovalControl, candidate ApprovalReconciliationCandidate, observation approvaleffect.Observation) (ApprovalReconciliationResult, error) {
	now := time.Now().UTC()
	if r.now != nil {
		now = r.now()
	}
	if observation.Validate() != nil || observation.ApprovalRequestID != candidate.RequestID || observation.CommandType != candidate.CommandType ||
		observation.CommandFingerprint != candidate.CommandFingerprint || observation.ObservedAt.Before(now.Add(-30*time.Second)) ||
		observation.ObservedAt.After(now.Add(30*time.Second)) {
		return ApprovalReconciliationBlockedEffectAuthority, nil
	}
	if observation.State != approvaleffect.StateAbsent {
		return ApprovalReconciliationBlockedEffect, nil
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	control, err := LockAdminApprovalControlShared(ctx, tx)
	if err != nil {
		return "", err
	}
	if !sameApprovalReconciliationControl(control, expectedControl) || (control.Mode != AdminApprovalModeQuiescing && control.Mode != AdminApprovalModeSafeRollback) {
		return "", ErrApprovalReconciliationControlChanged
	}
	outboxes, err := reconciliationOutboxesForUpdate(ctx, tx, candidate.RequestID)
	if err != nil {
		return "", err
	}
	if len(outboxes) != 1 {
		return ApprovalReconciliationBlockedHistory, nil
	}
	outbox := outboxes[0]
	if outbox.approvalEpoch > control.ApprovalEpoch {
		return ApprovalReconciliationBlockedHistory, nil
	}
	if outbox.deadLetteredAt != nil {
		if err := tx.QueryRow(ctx, `SELECT event_digest FROM public.inspect_authorization_dead_letter($1)`, outbox.eventID).Scan(&outbox.eventDigest); errors.Is(err, pgx.ErrNoRows) {
			return ApprovalReconciliationBlockedHistory, nil
		} else if err != nil {
			return "", err
		}
	}
	var commandType, commandFingerprint, executorIdentity, status string
	var policyVersion, version int64
	var executionDeadline, databaseNow time.Time
	err = tx.QueryRow(ctx, `SELECT r.command_type,encode(r.command_fingerprint,'hex'),r.policy_version,p.executor_identity,
  r.status,r.version,r.execution_deadline,transaction_timestamp()
FROM approval_requests r JOIN approval_command_policies p ON p.command_type=r.command_type AND p.policy_version=r.policy_version
WHERE r.id=$1 FOR UPDATE OF r`, candidate.RequestID).Scan(
		&commandType, &commandFingerprint, &policyVersion, &executorIdentity, &status, &version, &executionDeadline, &databaseNow)
	if errors.Is(err, pgx.ErrNoRows) {
		return ApprovalReconciliationStale, nil
	}
	if err != nil {
		return "", err
	}
	if commandType != candidate.CommandType || commandFingerprint != candidate.CommandFingerprint || policyVersion != candidate.PolicyVersion ||
		executorIdentity != candidate.ExecutorIdentity || status != candidate.Status ||
		version != candidate.Version || !executionDeadline.Equal(candidate.ExecutionDeadline) || executionDeadline.After(databaseNow) {
		return ApprovalReconciliationStale, nil
	}
	var executionExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM approval_executions WHERE approval_request_id=$1)`, candidate.RequestID).Scan(&executionExists); err != nil {
		return "", err
	}
	if executionExists {
		return ApprovalReconciliationBlockedExecution, nil
	}
	command, err := tx.Exec(ctx, `UPDATE approval_requests
SET status='INVALIDATED',invalidation_code='EXECUTION_WINDOW_EXPIRED',invalidation_details='{}',version=version+1,updated_at=$2
WHERE id=$1 AND version=$3 AND status=$4 AND command_type=$5 AND encode(command_fingerprint,'hex')=$6 AND policy_version=$7
  AND execution_deadline=$8 AND execution_deadline<=transaction_timestamp()
  AND NOT EXISTS(SELECT 1 FROM approval_executions WHERE approval_request_id=$1)`, candidate.RequestID, databaseNow,
		candidate.Version, candidate.Status, candidate.CommandType, candidate.CommandFingerprint, candidate.PolicyVersion, candidate.ExecutionDeadline)
	if err != nil {
		return "", err
	}
	if command.RowsAffected() != 1 {
		return ApprovalReconciliationStale, nil
	}
	if err := appendApprovalStateEvent(ctx, tx, candidate.RequestID, candidate.Status, "INVALIDATED", "SYSTEM", "approval-cutover-reconciler",
		"EXECUTION_WINDOW_EXPIRED", map[string]any{"effect_authority": observation.Authority, "effect_observed_at": observation.ObservedAt}, databaseNow); err != nil {
		return "", err
	}
	result := ApprovalReconciliationInvalidated
	if outbox.deadLetteredAt != nil {
		if outbox.publishedAt != nil || outbox.lockedBy != nil || outbox.lockedUntil != nil {
			return ApprovalReconciliationBlockedHistory, nil
		}
		auditID := uuid.Must(uuid.NewV7())
		auditRequestID := "approval-reconciliation-" + auditID.String()
		summary, err := json.Marshal(map[string]any{
			"request_id": candidate.RequestID.String(), "event_digest": outbox.eventDigest, "effect_absent": true,
			"effect_authority": observation.Authority, "effect_observed_at": observation.ObservedAt,
			"command_type": candidate.CommandType, "command_fingerprint": candidate.CommandFingerprint,
			"control_mode": control.Mode, "control_approval_epoch": control.ApprovalEpoch,
		})
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO audit_logs(id,action,resource_type,resource_id,result,request_id,trace_id,change_summary,occurred_at)
VALUES($1,'approval.authorization-exclusion.effect-verified','approval_authorization_event',$2,'SUCCEEDED',$3,$3,$4,$5)`,
			auditID, outbox.eventID.String(), auditRequestID, summary, databaseNow); err != nil {
			return "", err
		}
		reason := "CUTOVER_INVALIDATED_NO_EXECUTION"
		if control.Mode == AdminApprovalModeSafeRollback {
			reason = "SAFE_ROLLBACK_INVALIDATED_NO_EXECUTION"
		}
		if _, err := tx.Exec(ctx, `SELECT public.seal_approval_authorization_exclusion($1,$2,$3,$4)`,
			outbox.eventID, candidate.Version+1, reason, auditID); err != nil {
			return "", err
		}
		result = ApprovalReconciliationExclusionSealed
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return result, nil
}

func sameApprovalReconciliationControl(current, expected AdminApprovalControl) bool {
	return current.ControlID == expected.ControlID && current.SchemaVersion == expected.SchemaVersion && current.Mode == expected.Mode &&
		current.ApprovalWireVersion == expected.ApprovalWireVersion && current.ProofRequired == expected.ProofRequired &&
		current.AuthorizationRelayState == expected.AuthorizationRelayState && current.ApprovalEpoch == expected.ApprovalEpoch &&
		current.AuthorizationRelayEpoch == expected.AuthorizationRelayEpoch && current.Version == expected.Version &&
		current.UpdatedAt.Equal(expected.UpdatedAt)
}

func reconciliationOutboxesForUpdate(ctx context.Context, tx pgx.Tx, requestID uuid.UUID) ([]reconciliationOutbox, error) {
	rows, err := tx.Query(ctx, `SELECT o.event_id,o.approval_control_epoch,o.published_at,o.dead_lettered_at,o.locked_by,o.locked_until
FROM outbox_events o
WHERE o.aggregate_id=$1 AND o.event_type='account.approval-command.authorized.v1'
  AND o.approval_control_epoch IS NOT NULL AND o.approval_control_epoch>0
ORDER BY o.event_id FOR UPDATE OF o`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]reconciliationOutbox, 0, 2)
	for rows.Next() {
		var item reconciliationOutbox
		if err := rows.Scan(&item.eventID, &item.approvalEpoch, &item.publishedAt, &item.deadLetteredAt, &item.lockedBy, &item.lockedUntil); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func RunApprovalReconciliation(ctx context.Context, store ApprovalReconciliationStore, effects approvaleffect.Reader, limit int, apply bool) (ApprovalReconciliationSummary, error) {
	if store == nil || effects == nil {
		return ApprovalReconciliationSummary{}, ErrApprovalReconciliationBlocked
	}
	plan, err := store.PlanExpiredApprovalReconciliation(ctx, limit)
	if err != nil {
		return ApprovalReconciliationSummary{}, err
	}
	summary := ApprovalReconciliationSummary{Mode: plan.Control.Mode, ApprovalEpoch: plan.Control.ApprovalEpoch,
		ControlVersion: plan.Control.Version, CandidateCount: len(plan.Candidates), MoreCandidates: plan.MoreCandidates, DryRun: !apply}
	for _, candidate := range plan.Candidates {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		startedAt := time.Now().UTC()
		observation, observeErr := effects.Observe(ctx, candidate.RequestID, candidate.CommandType, candidate.CommandFingerprint)
		completedAt := time.Now().UTC()
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		if observeErr != nil || observation.Validate() != nil || observation.ApprovalRequestID != candidate.RequestID ||
			observation.CommandType != candidate.CommandType || observation.CommandFingerprint != candidate.CommandFingerprint ||
			observation.ObservedAt.Before(startedAt.Add(-30*time.Second)) || observation.ObservedAt.After(completedAt.Add(30*time.Second)) {
			summary.UnknownEffectCount++
			continue
		}
		if observation.State != approvaleffect.StateAbsent {
			summary.ObservedEffectCount++
			continue
		}
		summary.EligibleCount++
		if !apply {
			continue
		}
		result, reconcileErr := store.ReconcileExpiredApproval(ctx, plan.Control, candidate, observation)
		if reconcileErr != nil {
			return summary, reconcileErr
		}
		switch result {
		case ApprovalReconciliationInvalidated:
			summary.InvalidatedCount++
		case ApprovalReconciliationExclusionSealed:
			summary.InvalidatedCount++
			summary.SealedExclusionCount++
		case ApprovalReconciliationBlockedEffect:
			summary.ObservedEffectCount++
		case ApprovalReconciliationBlockedEffectAuthority:
			summary.UnknownEffectCount++
		case ApprovalReconciliationBlockedExecution:
			summary.ExecutionBlockedCount++
		case ApprovalReconciliationBlockedHistory:
			summary.HistoryBlockedCount++
		case ApprovalReconciliationStale:
			summary.StaleCount++
		default:
			return summary, ErrApprovalReconciliationBlocked
		}
	}
	summary.CompleteWithoutBlocker = summary.UnknownEffectCount == 0 && summary.ObservedEffectCount == 0 &&
		summary.ExecutionBlockedCount == 0 && summary.HistoryBlockedCount == 0 && summary.StaleCount == 0 &&
		!summary.MoreCandidates && (!apply || summary.InvalidatedCount == summary.EligibleCount)
	return summary, nil
}
