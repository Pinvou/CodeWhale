package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/google/uuid"
)

// The terminal invalidation may only ever be reached from the narrow, fully
// proven zero-effect case. Every guard is checked before the transaction, so a
// nil pool is enough to prove the fail-closed matrix without a database.
func TestInvalidateOriginalApprovalZeroEffectRejectsUnprovenCandidates(t *testing.T) {
	requestID, executionID, eventID, key := uuid.New(), uuid.New(), uuid.New(), "execution-key"
	candidate := ApprovalRecoveryCandidate{
		RequestID: requestID, ExecutionID: executionID, OriginalKey: key, OriginalEventID: eventID,
		Durable: "billing-ledger-repair-executor-p1-v1", Workload: approvalconsumer.BillingWorkload,
		CommandType: "LEDGER_REPAIR", CommandFingerprint: "fingerprint", PolicyVersion: 1, ApprovalEpoch: 1,
		RequestVersion: 3, RequestStatus: "EXECUTION_FAILED", ExecutionStatus: "FAILED",
		ExecutionDeadline: time.Now().UTC().Add(-time.Minute), WindowElapsed: true,
		Investigation: approvalconsumer.RecoveryAssessment{Class: approvalconsumer.FailurePermanent,
			Retryable: false, QueryOnly: true, MaxDomainAttempts: 1},
	}
	lease := RecoveryQueryLease{RequestID: requestID, Token: uuid.New(), Version: 2, QueryCount: 1}
	// A valid candidate plus a nil pool must still fail closed: an unwired
	// repository can never invalidate.
	repository := &PostgresRepository{}
	if err := repository.InvalidateOriginalApprovalZeroEffect(context.Background(), candidate, lease); !errors.Is(err, ErrApprovalRecoveryZeroEffectConflict) {
		t.Fatalf("unwired repository must fail closed, got %v", err)
	}
	for name, mutate := range map[string]func(*ApprovalRecoveryCandidate, *RecoveryQueryLease){
		"unknown classification": func(c *ApprovalRecoveryCandidate, _ *RecoveryQueryLease) {
			c.Investigation.Class = approvalconsumer.FailureUnknown
		},
		"window ended classification": func(c *ApprovalRecoveryCandidate, _ *RecoveryQueryLease) {
			c.Investigation.Class = approvalconsumer.FailureWindowEnded
		},
		"transient classification": func(c *ApprovalRecoveryCandidate, _ *RecoveryQueryLease) {
			c.Investigation.Class = approvalconsumer.FailureTransientSafe
		},
		"retryable": func(c *ApprovalRecoveryCandidate, _ *RecoveryQueryLease) { c.Investigation.Retryable = true },
		"not query only": func(c *ApprovalRecoveryCandidate, _ *RecoveryQueryLease) {
			c.Investigation.QueryOnly = false
		},
		"window open":           func(c *ApprovalRecoveryCandidate, _ *RecoveryQueryLease) { c.WindowElapsed = false },
		"missing deadline":      func(c *ApprovalRecoveryCandidate, _ *RecoveryQueryLease) { c.ExecutionDeadline = time.Time{} },
		"missing original key":  func(c *ApprovalRecoveryCandidate, _ *RecoveryQueryLease) { c.OriginalKey = "" },
		"stale request version": func(c *ApprovalRecoveryCandidate, _ *RecoveryQueryLease) { c.RequestVersion = 0 },
		"missing epoch":         func(c *ApprovalRecoveryCandidate, _ *RecoveryQueryLease) { c.ApprovalEpoch = 0 },
		"lease for another request": func(c *ApprovalRecoveryCandidate, l *RecoveryQueryLease) {
			l.RequestID = uuid.New()
		},
		"lease without token":   func(_ *ApprovalRecoveryCandidate, l *RecoveryQueryLease) { l.Token = uuid.Nil },
		"lease without version": func(_ *ApprovalRecoveryCandidate, l *RecoveryQueryLease) { l.Version = 0 },
		"foreign command binding": func(c *ApprovalRecoveryCandidate, _ *RecoveryQueryLease) {
			c.CommandType = "FUTURE_REPAIR"
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed, changedLease := candidate, lease
			mutate(&changed, &changedLease)
			if err := repository.InvalidateOriginalApprovalZeroEffect(context.Background(), changed, changedLease); !errors.Is(err, ErrApprovalRecoveryZeroEffectConflict) {
				t.Fatalf("%s must fail closed, got %v", name, err)
			}
		})
	}
}
