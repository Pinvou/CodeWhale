package account

import (
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvaleffect"
)

// The zero-effect verdict is the prerequisite for any later terminal
// transition, so it must be provable only from facts that are all
// independently authoritative, and every uncertain combination must fail
// closed. This is a pure table test: no database and no state change.
func TestRecoveryZeroEffectVerdictFailsClosed(t *testing.T) {
	definite := ApprovalRecoveryCandidate{
		ExecutionStatus:   "EXECUTION_FAILED",
		ExecutionDeadline: time.Now().UTC().Add(-time.Minute),
		WindowElapsed:     true,
		Investigation:     approvalconsumer.RecoveryAssessment{Class: approvalconsumer.FailurePermanent, Retryable: false, QueryOnly: true, MaxDomainAttempts: 1},
	}
	absent := ApprovalRecoveryInspection{EffectObservation: approvaleffect.StateAbsent, ResultObservation: "UNCONFIRMED"}
	if proven, reason := recoveryZeroEffectVerdict(definite, absent); !proven || reason != zeroEffectAbsentAfterDefiniteFailure {
		t.Fatalf("a definitely failed, window-ended, absent-effect candidate must be provable, got proven=%v reason=%s", proven, reason)
	}
	for name, mutate := range map[string]struct {
		candidate func(ApprovalRecoveryCandidate) ApprovalRecoveryCandidate
		observed  func(ApprovalRecoveryInspection) ApprovalRecoveryInspection
		reason    string
	}{
		"effect observed": {reason: zeroEffectEffectObserved,
			observed: func(value ApprovalRecoveryInspection) ApprovalRecoveryInspection {
				value.EffectObservation = approvaleffect.StateObserved
				return value
			}},
		"effect unavailable": {reason: zeroEffectEffectObserved,
			observed: func(value ApprovalRecoveryInspection) ApprovalRecoveryInspection {
				value.EffectObservation = "UNAVAILABLE"
				return value
			}},
		"positive result": {reason: zeroEffectPositiveResult,
			observed: func(value ApprovalRecoveryInspection) ApprovalRecoveryInspection {
				value.ResultObservation = "SUCCEEDED"
				return value
			}},
		"token pending result": {reason: zeroEffectPositiveResult,
			observed: func(value ApprovalRecoveryInspection) ApprovalRecoveryInspection {
				value.ResultObservation = "PENDING"
				return value
			}},
		"window open": {reason: zeroEffectWindowOpen,
			candidate: func(value ApprovalRecoveryCandidate) ApprovalRecoveryCandidate {
				value.WindowElapsed = false
				value.ExecutionDeadline = time.Now().UTC().Add(time.Minute)
				return value
			}},
		"missing deadline": {reason: zeroEffectWindowOpen,
			candidate: func(value ApprovalRecoveryCandidate) ApprovalRecoveryCandidate {
				value.ExecutionDeadline = time.Time{}
				return value
			}},
		"already executed": {reason: zeroEffectAlreadyTerminal,
			candidate: func(value ApprovalRecoveryCandidate) ApprovalRecoveryCandidate {
				value.ExecutionStatus = "EXECUTED"
				return value
			}},
		"already terminal": {reason: zeroEffectAlreadyTerminal,
			candidate: func(value ApprovalRecoveryCandidate) ApprovalRecoveryCandidate {
				value.ExecutionStatus = "TERMINAL_CONFIRMED"
				return value
			}},
		"unknown classification": {reason: zeroEffectClassNotDefinite,
			candidate: func(value ApprovalRecoveryCandidate) ApprovalRecoveryCandidate {
				value.Investigation.Class = approvalconsumer.FailureUnknown
				return value
			}},
		"window ended class": {reason: zeroEffectClassNotDefinite,
			candidate: func(value ApprovalRecoveryCandidate) ApprovalRecoveryCandidate {
				value.Investigation.Class = approvalconsumer.FailureWindowEnded
				return value
			}},
		"transient safe class": {reason: zeroEffectClassNotDefinite,
			candidate: func(value ApprovalRecoveryCandidate) ApprovalRecoveryCandidate {
				value.Investigation.Class = approvalconsumer.FailureTransientSafe
				return value
			}},
		"retryable investigation": {reason: zeroEffectInvestigationNotQueryOnly,
			candidate: func(value ApprovalRecoveryCandidate) ApprovalRecoveryCandidate {
				value.Investigation.Retryable = true
				return value
			}},
		"non query only investigation": {reason: zeroEffectInvestigationNotQueryOnly,
			candidate: func(value ApprovalRecoveryCandidate) ApprovalRecoveryCandidate {
				value.Investigation.QueryOnly = false
				return value
			}},
	} {
		t.Run(name, func(t *testing.T) {
			candidate, observation := definite, absent
			if mutate.candidate != nil {
				candidate = mutate.candidate(candidate)
			}
			if mutate.observed != nil {
				observation = mutate.observed(observation)
			}
			if proven, reason := recoveryZeroEffectVerdict(candidate, observation); proven || reason != mutate.reason {
				t.Fatalf("%s must stay unprovable: proven=%v reason=%s", name, proven, reason)
			}
		})
	}
	// A fact-or-policy change is equally definite and stays provable.
	changed := definite
	changed.Investigation.Class = approvalconsumer.FailureFactOrPolicyChanged
	if proven, _ := recoveryZeroEffectVerdict(changed, absent); !proven {
		t.Fatal("a definite fact/policy change with an absent effect must stay provable")
	}
}
