package account

import (
	"context"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvaleffect"
	"github.com/google/uuid"
)

type ApprovalRecoveryPlanner interface {
	PlanProofApprovalRecovery(context.Context, int) (ApprovalRecoveryPlan, error)
}

type ApprovalRecoveryInspection struct {
	Candidate         ApprovalRecoveryCandidate `json:"candidate"`
	EffectAuthority   string                    `json:"effect_authority,omitempty"`
	EffectObservation string                    `json:"effect_observation"`
	EffectObservedAt  *time.Time                `json:"effect_observed_at,omitempty"`
	// ABSENT is only a point-in-time negative observation, never proof that
	// an original attempt cannot still commit or an external probe had no effect.
	Disposition string `json:"disposition"`
}

type ApprovalRecoveryInspectionBatch struct {
	Plan        ApprovalRecoveryPlan         `json:"plan"`
	Inspections []ApprovalRecoveryInspection `json:"inspections"`
}

// InspectProofRecovery is intentionally query-only. It never interprets
// effect ABSENT as zero effect and never calls Account complete/invalidate,
// Billing/Token execute, or an external Provider. A domain result authority
// and durable fencing lease are still required for safe convergence.
func InspectProofRecovery(ctx context.Context, planner ApprovalRecoveryPlanner, effects approvaleffect.Reader, limit int) (ApprovalRecoveryInspectionBatch, error) {
	if planner == nil || effects == nil || limit < 1 || limit > 200 {
		return ApprovalRecoveryInspectionBatch{}, ErrApprovalRecoveryPlanInvalid
	}
	plan, err := planner.PlanProofApprovalRecovery(ctx, limit)
	if err != nil {
		return ApprovalRecoveryInspectionBatch{}, err
	}
	batch := ApprovalRecoveryInspectionBatch{Plan: plan, Inspections: make([]ApprovalRecoveryInspection, 0, len(plan.Candidates))}
	for _, candidate := range plan.Candidates {
		descriptor, ok := approvalconsumer.LookupDurable(candidate.Durable)
		if !ok || !approvalconsumer.Match(descriptor, candidate.CommandType, candidate.PolicyVersion, candidate.Workload) ||
			!approvaleffect.ValidFingerprint(candidate.CommandFingerprint) || candidate.RequestID == uuid.Nil ||
			candidate.OriginalEventID == uuid.Nil || candidate.ExecutionID == uuid.Nil ||
			candidate.Investigation.Retryable || !candidate.Investigation.QueryOnly {
			return ApprovalRecoveryInspectionBatch{}, ErrApprovalRecoveryPlanInvalid
		}
		entry := ApprovalRecoveryInspection{Candidate: candidate, EffectObservation: "UNAVAILABLE", Disposition: "UNKNOWN_QUERY_ONLY"}
		observation, observeErr := effects.Observe(ctx, candidate.RequestID, candidate.CommandType, candidate.CommandFingerprint)
		if observeErr != nil {
			if ctx.Err() != nil {
				return ApprovalRecoveryInspectionBatch{}, ctx.Err()
			}
			batch.Inspections = append(batch.Inspections, entry)
			continue
		}
		if observation.Validate() != nil || observation.ApprovalRequestID != candidate.RequestID ||
			observation.CommandType != candidate.CommandType || observation.CommandFingerprint != candidate.CommandFingerprint {
			return ApprovalRecoveryInspectionBatch{}, ErrApprovalRecoveryPlanInvalid
		}
		entry.EffectAuthority = observation.Authority
		entry.EffectObservation = observation.State
		observed := observation.ObservedAt
		entry.EffectObservedAt = &observed
		batch.Inspections = append(batch.Inspections, entry)
	}
	if err := ctx.Err(); err != nil {
		return ApprovalRecoveryInspectionBatch{}, err
	}
	return batch, nil
}
