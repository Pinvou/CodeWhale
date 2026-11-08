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
	ResultObservation string                    `json:"result_observation,omitempty"`
	ResultObservedAt  *time.Time                `json:"result_observed_at,omitempty"`
	// ABSENT is only a point-in-time negative observation, never proof that
	// an original attempt cannot still commit or an external probe had no effect.
	Disposition string `json:"disposition"`
	// ZeroEffectProven is a read-only verdict, never an authorization: it is
	// true only when the owner effect authority answered ABSENT under a live
	// lease, the owner result read proves no positive result, the failure class
	// is a definite pre-effect failure and the original execution window has
	// ended. A terminal transition still needs its own governed surface.
	ZeroEffectProven bool   `json:"zero_effect_proven"`
	ZeroEffectReason string `json:"zero_effect_reason,omitempty"`
}

const (
	zeroEffectAbsentAfterDefiniteFailure = "ABSENT_EFFECT_AFTER_DEFINITE_PRE_EFFECT_FAILURE"
	zeroEffectEffectObserved             = "EFFECT_OBSERVED_OR_UNAVAILABLE"
	zeroEffectPositiveResult             = "POSITIVE_RESULT_OBSERVED"
	zeroEffectWindowOpen                 = "WINDOW_OPEN"
	zeroEffectClassNotDefinite           = "CLASSIFICATION_NOT_DEFINITE"
	zeroEffectInvestigationNotQueryOnly  = "INVESTIGATION_NOT_QUERY_ONLY"
	zeroEffectAlreadyTerminal            = "EXECUTION_ALREADY_TERMINAL"
)

// recoveryZeroEffectVerdict derives the closed zero-effect verdict from facts
// the worker already gathered. It deliberately fails closed: anything uncertain
// (an observed effect, a positive/unknown result, an open window, an UNKNOWN
// classification or an execution that already reached a terminal state) keeps
// the candidate query-only.
func recoveryZeroEffectVerdict(candidate ApprovalRecoveryCandidate, observation ApprovalRecoveryInspection) (bool, string) {
	if observation.EffectObservation != approvaleffect.StateAbsent {
		return false, zeroEffectEffectObserved
	}
	if observation.ResultObservation != "" && observation.ResultObservation != "UNCONFIRMED" {
		return false, zeroEffectPositiveResult
	}
	if !candidate.WindowElapsed || candidate.ExecutionDeadline.IsZero() {
		return false, zeroEffectWindowOpen
	}
	if candidate.ExecutionStatus == "EXECUTED" || candidate.ExecutionStatus == "TERMINAL_CONFIRMED" {
		return false, zeroEffectAlreadyTerminal
	}
	if candidate.Investigation.Retryable || !candidate.Investigation.QueryOnly {
		return false, zeroEffectInvestigationNotQueryOnly
	}
	switch candidate.Investigation.Class {
	case approvalconsumer.FailurePermanent, approvalconsumer.FailureFactOrPolicyChanged:
	default:
		return false, zeroEffectClassNotDefinite
	}
	return true, zeroEffectAbsentAfterDefiniteFailure
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
	// One bounded inspection cannot silently run 200 independent five-second
	// authority requests for an unbounded period.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	plan, err := planner.PlanProofApprovalRecovery(ctx, limit)
	if err != nil {
		return ApprovalRecoveryInspectionBatch{}, err
	}
	if len(plan.Candidates) > limit {
		return ApprovalRecoveryInspectionBatch{}, ErrApprovalRecoveryPlanInvalid
	}
	batch := ApprovalRecoveryInspectionBatch{Plan: plan, Inspections: make([]ApprovalRecoveryInspection, 0, len(plan.Candidates))}
	for _, candidate := range plan.Candidates {
		if !validRecoveryInspectionCandidate(candidate) {
			return ApprovalRecoveryInspectionBatch{}, ErrApprovalRecoveryPlanInvalid
		}
		entry := ApprovalRecoveryInspection{Candidate: candidate, EffectObservation: "UNAVAILABLE", Disposition: "UNKNOWN_QUERY_ONLY"}
		started := time.Now().UTC()
		observation, observeErr := effects.Observe(ctx, candidate.RequestID, candidate.CommandType, candidate.CommandFingerprint)
		if observeErr != nil {
			if ctx.Err() != nil {
				return ApprovalRecoveryInspectionBatch{}, ctx.Err()
			}
			batch.Inspections = append(batch.Inspections, entry)
			continue
		}
		if observation.Validate() != nil || observation.ObservedAt.Before(started.Add(-30*time.Second)) ||
			observation.ObservedAt.After(time.Now().UTC().Add(30*time.Second)) || observation.ApprovalRequestID != candidate.RequestID ||
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

func validRecoveryInspectionCandidate(candidate ApprovalRecoveryCandidate) bool {
	descriptor, ok := approvalconsumer.LookupDurable(candidate.Durable)
	return ok && approvalconsumer.Match(descriptor, candidate.CommandType, candidate.PolicyVersion, candidate.Workload) &&
		approvaleffect.ValidFingerprint(candidate.CommandFingerprint) && candidate.RequestID != uuid.Nil &&
		candidate.OriginalEventID != uuid.Nil && candidate.ExecutionID != uuid.Nil &&
		!candidate.Investigation.Retryable && candidate.Investigation.QueryOnly && candidate.Investigation.MaxDomainAttempts == 1
}
