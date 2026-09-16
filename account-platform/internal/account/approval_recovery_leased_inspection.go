package account

import (
	"context"
	"errors"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvaleffect"
	"github.com/google/uuid"
)

type RecoveryQueryLeaser interface {
	Claim(context.Context, ApprovalRecoveryCandidate) (RecoveryQueryLease, error)
	Release(context.Context, RecoveryQueryLease) (time.Time, error)
}

type oneRecoveryCandidate struct{ candidate ApprovalRecoveryCandidate }

func (p oneRecoveryCandidate) PlanProofApprovalRecovery(context.Context, int) (ApprovalRecoveryPlan, error) {
	return ApprovalRecoveryPlan{Candidates: []ApprovalRecoveryCandidate{p.candidate}}, nil
}

// InspectProofRecoveryLeased performs only read-only effect queries, each
// protected by a durable short lease on the original execution. Even an
// ABSENT observation leaves UNKNOWN_QUERY_ONLY: it cannot prove zero effect.
// This is not a terminal recovery worker or a retry authorization.
func InspectProofRecoveryLeased(ctx context.Context, planner ApprovalRecoveryPlanner, leaser RecoveryQueryLeaser, effects approvaleffect.Reader, limit int) (ApprovalRecoveryInspectionBatch, error) {
	if planner == nil || leaser == nil || effects == nil || limit < 1 || limit > 200 {
		return ApprovalRecoveryInspectionBatch{}, ErrApprovalRecoveryPlanInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	plan, err := planner.PlanProofApprovalRecovery(ctx, limit)
	if err != nil {
		return ApprovalRecoveryInspectionBatch{}, err
	}
	if len(plan.Candidates) > limit {
		return ApprovalRecoveryInspectionBatch{}, ErrApprovalRecoveryPlanInvalid
	}
	seen := make(map[uuid.UUID]struct{}, len(plan.Candidates))
	for _, candidate := range plan.Candidates {
		if !validRecoveryInspectionCandidate(candidate) || candidate.OriginalKey == "" || candidate.ApprovalEpoch < 1 || candidate.RequestVersion < 1 {
			return ApprovalRecoveryInspectionBatch{}, ErrApprovalRecoveryPlanInvalid
		}
		if _, duplicate := seen[candidate.RequestID]; duplicate {
			return ApprovalRecoveryInspectionBatch{}, ErrApprovalRecoveryPlanInvalid
		}
		seen[candidate.RequestID] = struct{}{}
	}
	batch := ApprovalRecoveryInspectionBatch{Plan: plan, Inspections: make([]ApprovalRecoveryInspection, 0, len(plan.Candidates))}
	for _, candidate := range plan.Candidates {
		lease, claimErr := leaser.Claim(ctx, candidate)
		if errors.Is(claimErr, ErrApprovalRecoveryLeaseUnavailable) {
			batch.Inspections = append(batch.Inspections, ApprovalRecoveryInspection{
				Candidate: candidate, EffectObservation: "UNAVAILABLE", Disposition: "UNKNOWN_QUERY_ONLY",
			})
			continue
		}
		if claimErr != nil {
			return ApprovalRecoveryInspectionBatch{}, claimErr
		}
		if lease.RequestID != candidate.RequestID || lease.Token == uuid.Nil || lease.Version < 1 || lease.QueryCount < 1 {
			return ApprovalRecoveryInspectionBatch{}, ErrApprovalRecoveryPlanInvalid
		}
		deadline := time.Now().Add(10 * time.Second)
		if lease.Until.Add(-time.Second).Before(deadline) {
			deadline = lease.Until.Add(-time.Second)
		}
		var observation ApprovalRecoveryInspection
		var inspectionErr error
		if deadline.After(time.Now()) {
			queryCtx, stop := context.WithDeadline(ctx, deadline)
			one, err := InspectProofRecovery(queryCtx, oneRecoveryCandidate{candidate}, effects, 1)
			stop()
			inspectionErr = err
			if err == nil {
				observation = one.Inspections[0]
			}
		} else {
			inspectionErr = ErrApprovalRecoveryLeaseUnavailable
		}
		// Release uses a fresh bounded context so cancellation cannot silently
		// retain a live lease. A failed release still cannot authorize a retry.
		releaseCtx, stop := context.WithTimeout(context.Background(), 2*time.Second)
		_, releaseErr := leaser.Release(releaseCtx, lease)
		stop()
		if releaseErr != nil {
			return ApprovalRecoveryInspectionBatch{}, releaseErr
		}
		if inspectionErr != nil {
			return ApprovalRecoveryInspectionBatch{}, inspectionErr
		}
		batch.Inspections = append(batch.Inspections, observation)
	}
	return batch, nil
}
