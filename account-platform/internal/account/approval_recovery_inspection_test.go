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
}
