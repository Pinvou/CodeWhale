package account

import (
	"context"
	"errors"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrApprovalRecoveryPlanInvalid = errors.New("approval recovery plan has inconsistent evidence")

// ApprovalRecoveryCandidate contains only references and stable codes. It is
// not an execution lease or an authorization to retry or invalidate anything.
type ApprovalRecoveryCandidate struct {
	RequestID         uuid.UUID                           `json:"approval_request_id"`
	ExecutionID       uuid.UUID                           `json:"execution_id"`
	OriginalKey       string                              `json:"original_execution_idempotency_key"`
	OriginalEventID   uuid.UUID                           `json:"original_event_id"`
	Durable           string                              `json:"durable"`
	Workload          string                              `json:"workload"`
	CommandType       string                              `json:"command_type"`
	PolicyVersion     int64                               `json:"policy_version"`
	ApprovalEpoch     int64                               `json:"approval_epoch"`
	RequestStatus     string                              `json:"request_status"`
	ExecutionStatus   string                              `json:"execution_status"`
	ExecutionDeadline time.Time                           `json:"execution_deadline"`
	WindowElapsed     bool                                `json:"window_elapsed"`
	Delivery          ApprovalDeliveryState               `json:"delivery"`
	Investigation     approvalconsumer.RecoveryAssessment `json:"investigation"`
}

type ApprovalRecoveryPlan struct {
	ObservedAt     time.Time                   `json:"observed_at"`
	Control        AdminApprovalControl        `json:"control"`
	Candidates     []ApprovalRecoveryCandidate `json:"candidates"`
	MoreCandidates bool                        `json:"more_candidates"`
}

// PlanProofApprovalRecovery reads a bounded, consistent Account snapshot.
// It never calls the domain, takes a lease, completes an execution or changes
// a request. A separate least-privilege worker and domain result authority
// must be installed before this plan can drive any terminal transition.
func (r *PostgresRepository) PlanProofApprovalRecovery(ctx context.Context, limit int) (ApprovalRecoveryPlan, error) {
	if limit < 1 || limit > 200 {
		return ApprovalRecoveryPlan{}, ErrApprovalRecoveryPlanInvalid
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ApprovalRecoveryPlan{}, err
	}
	defer tx.Rollback(ctx)
	plan := ApprovalRecoveryPlan{Candidates: make([]ApprovalRecoveryCandidate, 0)}
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&plan.ObservedAt); err != nil {
		return ApprovalRecoveryPlan{}, err
	}
	plan.Control, err = ReadAdminApprovalControl(ctx, tx)
	if err != nil {
		return ApprovalRecoveryPlan{}, err
	}
	rows, err := tx.Query(ctx, `SELECT r.id,e.id,e.execution_idempotency_key,e.authorization_event_id,e.authorization_durable,e.executor_identity,
 r.command_type,r.policy_version,e.authorization_approval_epoch,r.status,e.status,r.execution_deadline
FROM public.approval_requests r JOIN public.approval_executions e ON e.approval_request_id=r.id AND e.command_type=r.command_type
WHERE e.authorization_serialization_version='APPROVAL_RFC8785_DECIMAL_V1'
  AND e.status IN ('EXECUTING','FAILED') AND r.status IN ('EXECUTING','EXECUTION_FAILED')
-- Existing approval_requests_queue_idx begins with status,expires_at,updated_at,id.
-- Drive the bounded scan from that index rather than sorting all executions.
ORDER BY r.status,r.expires_at,r.updated_at,r.id LIMIT $1`, limit+1)
	if err != nil {
		return ApprovalRecoveryPlan{}, err
	}
	for rows.Next() {
		var item ApprovalRecoveryCandidate
		if err := rows.Scan(&item.RequestID, &item.ExecutionID, &item.OriginalKey, &item.OriginalEventID, &item.Durable,
			&item.Workload, &item.CommandType, &item.PolicyVersion, &item.ApprovalEpoch,
			&item.RequestStatus, &item.ExecutionStatus, &item.ExecutionDeadline); err != nil {
			rows.Close()
			return ApprovalRecoveryPlan{}, err
		}
		if len(plan.Candidates) < limit {
			plan.Candidates = append(plan.Candidates, item)
		} else {
			plan.MoreCandidates = true
		}
	}
	readErr := rows.Err()
	rows.Close()
	if readErr != nil {
		return ApprovalRecoveryPlan{}, readErr
	}
	for index := range plan.Candidates {
		item := &plan.Candidates[index]
		descriptor, ok := approvalconsumer.LookupDurable(item.Durable)
		if !ok || !approvalconsumer.Match(descriptor, item.CommandType, item.PolicyVersion, item.Workload) ||
			item.RequestID == uuid.Nil || item.ExecutionID == uuid.Nil || item.OriginalEventID == uuid.Nil || item.OriginalKey == "" ||
			item.ApprovalEpoch < 1 || item.ExecutionDeadline.IsZero() ||
			(item.RequestStatus == "EXECUTING" && item.ExecutionStatus != "EXECUTING") ||
			(item.RequestStatus == "EXECUTION_FAILED" && item.ExecutionStatus != "FAILED") {
			return ApprovalRecoveryPlan{}, ErrApprovalRecoveryPlanInvalid
		}
		item.Delivery, err = readRecoveryDeliveryState(ctx, tx, *item)
		if err != nil {
			return ApprovalRecoveryPlan{}, err
		}
		if item.Delivery.ExecutionStatus == nil || *item.Delivery.ExecutionStatus != item.ExecutionStatus ||
			(item.Workload == approvalconsumer.BillingWorkload && item.Delivery.DomainAttemptCount < 1) {
			return ApprovalRecoveryPlan{}, ErrApprovalRecoveryPlanInvalid
		}
		code := ""
		if item.Delivery.LastErrorCode != nil {
			code = *item.Delivery.LastErrorCode
		}
		if item.Delivery.ExecutionErrorClass != nil && code != "" && code != *item.Delivery.ExecutionErrorClass {
			// Competing codes are not evidence that either is authoritative.
			code = ""
		}
		if code == "" && item.Delivery.ExecutionErrorClass != nil && item.Delivery.LastErrorCode == nil {
			code = *item.Delivery.ExecutionErrorClass
		}
		item.Investigation = approvalconsumer.ClassifyProofFailure(item.CommandType, item.PolicyVersion, code)
		item.ExecutionDeadline = item.ExecutionDeadline.UTC()
		item.WindowElapsed = !item.ExecutionDeadline.After(plan.ObservedAt)
	}
	if err := tx.Commit(ctx); err != nil {
		return ApprovalRecoveryPlan{}, err
	}
	plan.ObservedAt = plan.ObservedAt.UTC()
	return plan, nil
}

func readRecoveryDeliveryState(ctx context.Context, tx pgx.Tx, item ApprovalRecoveryCandidate) (ApprovalDeliveryState, error) {
	var state ApprovalDeliveryState
	err := tx.QueryRow(ctx, `SELECT domain_attempt_count,state,version,last_error_code,execution_status,execution_error_class
FROM public.read_approval_authorization_delivery_state($1,$2,$3)`, item.OriginalEventID, item.Durable, item.Workload).Scan(
		&state.DomainAttemptCount, &state.State, &state.Version, &state.LastErrorCode, &state.ExecutionStatus, &state.ExecutionErrorClass,
	)
	if err != nil {
		classified := approvalConsumerError(ctx, err)
		if errors.Is(classified, ErrApprovalConsumerBindingInvalid) {
			return ApprovalDeliveryState{}, ErrApprovalRecoveryPlanInvalid
		}
		return ApprovalDeliveryState{}, classified
	}
	return state, nil
}
