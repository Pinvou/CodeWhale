package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvaleffect"
	"github.com/google/uuid"
)

type recoveryPlannerStub struct {
	plan  ApprovalRecoveryPlan
	calls int
}

func (s *recoveryPlannerStub) PlanProofApprovalRecovery(context.Context, int) (ApprovalRecoveryPlan, error) {
	s.calls++
	return s.plan, nil
}

type recoveryEffectStub struct {
	state string
	err   error
	calls int
}

type recoveryLeaserStub struct {
	claimErr     error
	leaseUntil   time.Time
	claimCalls   int
	releaseCalls int
}

func (s *recoveryLeaserStub) Claim(_ context.Context, candidate ApprovalRecoveryCandidate) (RecoveryQueryLease, error) {
	s.claimCalls++
	if s.claimErr != nil {
		return RecoveryQueryLease{}, s.claimErr
	}
	until := s.leaseUntil
	if until.IsZero() {
		until = time.Now().Add(15 * time.Second)
	}
	return RecoveryQueryLease{RequestID: candidate.RequestID, Token: uuid.New(), Until: until, QueryCount: 1, Version: 1}, nil
}

func (s *recoveryLeaserStub) Release(context.Context, RecoveryQueryLease) (time.Time, error) {
	s.releaseCalls++
	return time.Now().Add(2 * time.Second), nil
}

func (s *recoveryEffectStub) Observe(_ context.Context, requestID uuid.UUID, command, fingerprint string) (approvaleffect.Observation, error) {
	s.calls++
	if s.err != nil {
		return approvaleffect.Observation{}, s.err
	}
	return approvaleffect.Observation{
		SchemaVersion: approvaleffect.SchemaVersion, Authority: approvaleffect.AuthorityBilling,
		ApprovalRequestID: requestID, CommandType: command, CommandFingerprint: fingerprint,
		State: s.state, ObservedAt: time.Now().UTC(),
	}, nil
}

func TestProofRecoveryInspectionNeverPromotesAbsenceToZeroEffect(t *testing.T) {
	descriptor := approvalconsumer.BillingCommands()[0]
	item := ApprovalRecoveryCandidate{
		RequestID: uuid.New(), ExecutionID: uuid.New(), OriginalEventID: uuid.New(),
		Durable: descriptor.Durable, Workload: descriptor.Workload,
		CommandType: descriptor.CommandType, PolicyVersion: descriptor.PolicyVersion,
		CommandFingerprint: "a1" + "00000000000000000000000000000000000000000000000000000000000000",
		Investigation:      approvalconsumer.ClassifyProofFailure(descriptor.CommandType, descriptor.PolicyVersion, "DOMAIN_REVISION_CHANGED"),
	}
	for _, test := range []struct {
		name, state, want string
		err               error
	}{
		{"absent", approvaleffect.StateAbsent, approvaleffect.StateAbsent, nil},
		{"observed", approvaleffect.StateObserved, approvaleffect.StateObserved, nil},
		{"unavailable", "", "UNAVAILABLE", errors.New("authority down")},
	} {
		t.Run(test.name, func(t *testing.T) {
			planner := &recoveryPlannerStub{plan: ApprovalRecoveryPlan{Candidates: []ApprovalRecoveryCandidate{item}}}
			effects := &recoveryEffectStub{state: test.state, err: test.err}
			batch, err := InspectProofRecovery(context.Background(), planner, effects, 1)
			if err != nil || planner.calls != 1 || effects.calls != 1 || len(batch.Inspections) != 1 ||
				batch.Inspections[0].EffectObservation != test.want || batch.Inspections[0].Disposition != "UNKNOWN_QUERY_ONLY" {
				t.Fatalf("inspection=%+v planner=%d effects=%d err=%v", batch, planner.calls, effects.calls, err)
			}
		})
	}
	bad := item
	bad.Durable = "other-command"
	planner := &recoveryPlannerStub{plan: ApprovalRecoveryPlan{Candidates: []ApprovalRecoveryCandidate{bad}}}
	effects := &recoveryEffectStub{state: approvaleffect.StateAbsent}
	if _, err := InspectProofRecovery(context.Background(), planner, effects, 1); !errors.Is(err, ErrApprovalRecoveryPlanInvalid) || effects.calls != 0 {
		t.Fatalf("forged candidate reached domain: err=%v calls=%d", err, effects.calls)
	}
	planner.plan.Candidates = []ApprovalRecoveryCandidate{item, item}
	if _, err := InspectProofRecovery(context.Background(), planner, effects, 1); !errors.Is(err, ErrApprovalRecoveryPlanInvalid) || effects.calls != 0 {
		t.Fatalf("oversized plan reached domain: err=%v calls=%d", err, effects.calls)
	}
}

func TestProofRecoveryLeasedInspectionOnlyQueriesUnderLease(t *testing.T) {
	descriptor := approvalconsumer.BillingCommands()[0]
	item := ApprovalRecoveryCandidate{
		RequestID: uuid.New(), ExecutionID: uuid.New(), OriginalEventID: uuid.New(), OriginalKey: "original-key",
		Durable: descriptor.Durable, Workload: descriptor.Workload,
		CommandType: descriptor.CommandType, PolicyVersion: descriptor.PolicyVersion,
		CommandFingerprint: "a1" + "00000000000000000000000000000000000000000000000000000000000000",
		ApprovalEpoch:      1, RequestVersion: 1,
		Investigation: approvalconsumer.ClassifyProofFailure(descriptor.CommandType, descriptor.PolicyVersion, "DOMAIN_REVISION_CHANGED"),
	}
	for _, tc := range []struct {
		name           string
		claimErr       error
		effectErr      error
		until          time.Time
		wantEffectCall int
		wantRelease    int
		wantErr        error
	}{
		{name: "absent stays unknown", wantEffectCall: 1, wantRelease: 1},
		{name: "held lease skips authority", claimErr: ErrApprovalRecoveryLeaseUnavailable},
		{name: "authority down releases", effectErr: errors.New("authority down"), wantEffectCall: 1, wantRelease: 1},
		{name: "expired lease releases", until: time.Now().Add(-time.Second), wantRelease: 1, wantErr: ErrApprovalRecoveryLeaseUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			planner := &recoveryPlannerStub{plan: ApprovalRecoveryPlan{Candidates: []ApprovalRecoveryCandidate{item}}}
			leaser := &recoveryLeaserStub{claimErr: tc.claimErr, leaseUntil: tc.until}
			effects := &recoveryEffectStub{state: approvaleffect.StateAbsent, err: tc.effectErr}
			batch, err := InspectProofRecoveryLeased(context.Background(), planner, leaser, effects, 1)
			if !errors.Is(err, tc.wantErr) || effects.calls != tc.wantEffectCall || leaser.releaseCalls != tc.wantRelease || leaser.claimCalls != 1 {
				t.Fatalf("leased inspection=%+v err=%v effect=%d claim=%d release=%d", batch, err, effects.calls, leaser.claimCalls, leaser.releaseCalls)
			}
			if err == nil && (len(batch.Inspections) != 1 || batch.Inspections[0].Disposition != "UNKNOWN_QUERY_ONLY") {
				t.Fatalf("inspection unexpectedly authorized recovery: %+v", batch)
			}
		})
	}
	bad := item
	bad.Durable = "forged"
	leaser := &recoveryLeaserStub{}
	effects := &recoveryEffectStub{state: approvaleffect.StateAbsent}
	if _, err := InspectProofRecoveryLeased(context.Background(), &recoveryPlannerStub{plan: ApprovalRecoveryPlan{Candidates: []ApprovalRecoveryCandidate{bad}}}, leaser, effects, 1); !errors.Is(err, ErrApprovalRecoveryPlanInvalid) || leaser.claimCalls != 0 || effects.calls != 0 {
		t.Fatalf("forged candidate reached lease/effect: %v, %d, %d", err, leaser.claimCalls, effects.calls)
	}
}
