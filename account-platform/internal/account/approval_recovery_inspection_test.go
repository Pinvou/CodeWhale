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

type recoveryResultStub struct {
	state string
	err   error
	calls int
}

type recoveryConfirmerStub struct {
	calls int
	err   error
}

func (stub *recoveryConfirmerStub) ConfirmOriginalSuccess(_ context.Context, _ ApprovalRecoveryCandidate, _ RecoveryQueryLease) error {
	stub.calls++
	return stub.err
}

func (stub *recoveryResultStub) ObserveRecoveryResult(_ context.Context, _ ApprovalRecoveryCandidate) (RecoveryResultObservation, error) {
	stub.calls++
	if stub.err != nil {
		return RecoveryResultObservation{}, stub.err
	}
	return RecoveryResultObservation{State: stub.state, ObservedAt: time.Now().UTC()}, nil
}

func TestProofRecoveryResultAndEffectStayUnderOneLeaseAndNeverAuthorizeMutation(t *testing.T) {
	descriptor := approvalconsumer.BillingCommands()[0]
	item := ApprovalRecoveryCandidate{RequestID: uuid.New(), ExecutionID: uuid.New(), OriginalEventID: uuid.New(), OriginalKey: "original-key",
		Durable: descriptor.Durable, Workload: descriptor.Workload, CommandType: descriptor.CommandType,
		PolicyVersion: descriptor.PolicyVersion, CommandFingerprint: "a1" + "00000000000000000000000000000000000000000000000000000000000000",
		ApprovalEpoch: 1, RequestVersion: 1, Investigation: approvalconsumer.ClassifyProofFailure(descriptor.CommandType, descriptor.PolicyVersion, "")}
	for _, tc := range []struct {
		name, result, effect string
		resultErr            error
		wantErr              error
		wantResult           string
	}{
		{"confirmed", "SUCCEEDED", approvaleffect.StateObserved, nil, nil, "SUCCEEDED"},
		{"unconfirmed", "UNCONFIRMED", approvaleffect.StateAbsent, nil, nil, "UNCONFIRMED"},
		{"result unavailable", "", approvaleffect.StateAbsent, errors.New("down"), nil, "UNAVAILABLE"},
		{"contradictory success", "SUCCEEDED", approvaleffect.StateAbsent, nil, ErrApprovalRecoveryPlanInvalid, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leaser := &recoveryLeaserStub{}
			effects := &recoveryEffectStub{state: tc.effect}
			results := &recoveryResultStub{state: tc.result, err: tc.resultErr}
			batch, err := InspectProofRecoveryLeasedWithResults(context.Background(), &recoveryPlannerStub{plan: ApprovalRecoveryPlan{Candidates: []ApprovalRecoveryCandidate{item}}}, leaser, effects, results, 1)
			if !errors.Is(err, tc.wantErr) || leaser.claimCalls != 1 || leaser.releaseCalls != 1 || results.calls != 1 || effects.calls != 1 {
				t.Fatalf("batch=%+v err=%v claim=%d release=%d result=%d effect=%d", batch, err, leaser.claimCalls, leaser.releaseCalls, results.calls, effects.calls)
			}
			if err == nil && (batch.Inspections[0].ResultObservation != tc.wantResult || batch.Inspections[0].Disposition != "UNKNOWN_QUERY_ONLY") {
				t.Fatalf("result observation was promoted to recovery: %+v", batch.Inspections[0])
			}
		})
	}
}

func TestProofRecoveryConvergesOnlyPositiveOriginalResultUnderLease(t *testing.T) {
	descriptor := approvalconsumer.BillingCommands()[0]
	item := ApprovalRecoveryCandidate{RequestID: uuid.New(), ExecutionID: uuid.New(), OriginalEventID: uuid.New(), OriginalKey: "original-key",
		Durable: descriptor.Durable, Workload: descriptor.Workload, CommandType: descriptor.CommandType,
		PolicyVersion: descriptor.PolicyVersion, CommandFingerprint: "a1" + "00000000000000000000000000000000000000000000000000000000000000",
		ApprovalEpoch: 1, RequestVersion: 1, Investigation: approvalconsumer.ClassifyProofFailure(descriptor.CommandType, descriptor.PolicyVersion, "")}
	for _, tc := range []struct {
		name, result, effect, wantDisposition string
		confirmErr                            error
		wantConfirm                           int
		wantError                             bool
	}{
		{"positive", "SUCCEEDED", approvaleffect.StateObserved, "ORIGINAL_RESULT_CONFIRMED", nil, 1, false},
		{"unconfirmed", "UNCONFIRMED", approvaleffect.StateAbsent, "UNKNOWN_QUERY_ONLY", nil, 0, false},
		{"contradiction", "SUCCEEDED", approvaleffect.StateAbsent, "", nil, 0, true},
		{"account unavailable", "SUCCEEDED", approvaleffect.StateObserved, "", ErrApprovalRecoveryConfirmationUnavailable, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leaser := &recoveryLeaserStub{}
			confirmer := &recoveryConfirmerStub{err: tc.confirmErr}
			batch, err := InspectProofRecoveryLeasedWithConfirmation(context.Background(), &recoveryPlannerStub{plan: ApprovalRecoveryPlan{Candidates: []ApprovalRecoveryCandidate{item}}},
				leaser, &recoveryEffectStub{state: tc.effect}, &recoveryResultStub{state: tc.result}, confirmer, 1)
			if (err != nil) != tc.wantError || confirmer.calls != tc.wantConfirm || leaser.releaseCalls != 1 {
				t.Fatalf("err=%v confirmer=%d releases=%d", err, confirmer.calls, leaser.releaseCalls)
			}
			if err == nil && batch.Inspections[0].Disposition != tc.wantDisposition {
				t.Fatalf("disposition=%s", batch.Inspections[0].Disposition)
			}
		})
	}
}
