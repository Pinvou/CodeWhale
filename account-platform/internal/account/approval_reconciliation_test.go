package account

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvaleffect"
	"github.com/google/uuid"
)

type reconciliationStoreStub struct {
	plan    ApprovalReconciliationPlan
	results map[uuid.UUID]ApprovalReconciliationResult
	calls   int
}

func (stub *reconciliationStoreStub) PlanApprovalReconciliation(context.Context, int) (ApprovalReconciliationPlan, error) {
	return stub.plan, nil
}

func (stub *reconciliationStoreStub) ReconcileApproval(_ context.Context, _ AdminApprovalControl, candidate ApprovalReconciliationCandidate, _ approvaleffect.Observation) (ApprovalReconciliationResult, error) {
	stub.calls++
	return stub.results[candidate.RequestID], nil
}

type reconciliationEffectStub struct {
	states map[uuid.UUID]string
	errors map[uuid.UUID]error
}

func (stub reconciliationEffectStub) Observe(_ context.Context, requestID uuid.UUID, commandType, fingerprint string) (approvaleffect.Observation, error) {
	if err := stub.errors[requestID]; err != nil {
		return approvaleffect.Observation{}, err
	}
	state := stub.states[requestID]
	if state == "" {
		state = approvaleffect.StateAbsent
	}
	authority, _ := approvaleffect.AuthorityForCommand(commandType)
	return approvaleffect.Observation{SchemaVersion: 1, Authority: authority, ApprovalRequestID: requestID,
		CommandType: commandType, CommandFingerprint: fingerprint, State: state, ObservedAt: time.Now().UTC()}, nil
}

func TestRunApprovalReconciliationClassifiesEveryFailClosedOutcome(t *testing.T) {
	control := validAdminApprovalControl(AdminApprovalModeQuiescing, AdminApprovalRelayRunning)
	fingerprint := strings.Repeat("a", 64)
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()}
	candidates := make([]ApprovalReconciliationCandidate, 0, len(ids))
	for _, id := range ids {
		candidates = append(candidates, ApprovalReconciliationCandidate{RequestID: id, CommandType: "WALLET_ADJUSTMENT",
			CommandFingerprint: fingerprint, Status: "APPROVED", Version: 2, ExecutionDeadline: time.Now().UTC().Add(-time.Minute),
			InvalidationCode: "EXECUTION_WINDOW_EXPIRED"})
	}
	store := &reconciliationStoreStub{plan: ApprovalReconciliationPlan{Control: control, Candidates: candidates}, results: map[uuid.UUID]ApprovalReconciliationResult{
		ids[0]: ApprovalReconciliationInvalidated, ids[1]: ApprovalReconciliationExclusionSealed,
		ids[4]: ApprovalReconciliationBlockedExecution, ids[5]: ApprovalReconciliationStale,
	}}
	effects := reconciliationEffectStub{states: map[uuid.UUID]string{ids[2]: approvaleffect.StateObserved}, errors: map[uuid.UUID]error{ids[3]: errors.New("unavailable")}}
	summary, err := RunApprovalReconciliation(context.Background(), store, effects, 20, true)
	if err != nil {
		t.Fatal(err)
	}
	if summary.InvalidatedCount != 2 || summary.SealedExclusionCount != 1 || summary.ObservedEffectCount != 1 ||
		summary.UnknownEffectCount != 1 || summary.ExecutionBlockedCount != 1 || summary.StaleCount != 1 ||
		summary.ExpiredCandidateCount != len(ids) || summary.SafeCandidateCount != 0 || summary.CompleteWithoutBlocker || store.calls != 4 {
		t.Fatalf("unexpected summary=%+v calls=%d", summary, store.calls)
	}
	store.calls = 0
	dryRun, err := RunApprovalReconciliation(context.Background(), store, reconciliationEffectStub{}, 20, false)
	if err != nil || dryRun.EligibleCount != len(ids) || dryRun.InvalidatedCount != 0 || !dryRun.DryRun || store.calls != 0 {
		t.Fatalf("dry-run summary=%+v calls=%d err=%v", dryRun, store.calls, err)
	}
}

func TestRunApprovalReconciliationCountsSafeConvergenceSeparately(t *testing.T) {
	control := validAdminApprovalControl(AdminApprovalModeSafeRollback, AdminApprovalRelayRunning)
	id := uuid.New()
	candidate := ApprovalReconciliationCandidate{RequestID: id, CommandType: "WALLET_ADJUSTMENT",
		CommandFingerprint: strings.Repeat("b", 64), Status: "APPROVED", Version: 3,
		ExecutionDeadline: time.Now().UTC().Add(time.Minute), InvalidationCode: "APPROVAL_SAFE_ROLLBACK_INVALIDATED"}
	store := &reconciliationStoreStub{plan: ApprovalReconciliationPlan{Control: control, Candidates: []ApprovalReconciliationCandidate{candidate}},
		results: map[uuid.UUID]ApprovalReconciliationResult{id: ApprovalReconciliationInvalidated}}
	summary, err := RunApprovalReconciliation(context.Background(), store, reconciliationEffectStub{}, 1, true)
	if err != nil || summary.SafeCandidateCount != 1 || summary.ExpiredCandidateCount != 0 || summary.InvalidatedCount != 1 ||
		!summary.CompleteWithoutBlocker {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
}

func TestRunApprovalReconciliationNeverClaimsATruncatedBatchComplete(t *testing.T) {
	control := validAdminApprovalControl(AdminApprovalModeQuiescing, AdminApprovalRelayRunning)
	store := &reconciliationStoreStub{plan: ApprovalReconciliationPlan{Control: control, MoreCandidates: true}}
	summary, err := RunApprovalReconciliation(context.Background(), store, reconciliationEffectStub{}, 20, false)
	if err != nil || !summary.MoreCandidates || summary.CompleteWithoutBlocker {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
}

func TestReconcileExpiredApprovalRejectsUnboundObservationBeforeDatabaseAccess(t *testing.T) {
	candidate := ApprovalReconciliationCandidate{RequestID: uuid.New(), CommandType: "WALLET_ADJUSTMENT",
		CommandFingerprint: strings.Repeat("a", 64)}
	observation := approvaleffect.Observation{SchemaVersion: 1, Authority: approvaleffect.AuthorityBilling,
		ApprovalRequestID: uuid.New(), CommandType: candidate.CommandType, CommandFingerprint: candidate.CommandFingerprint,
		State: approvaleffect.StateAbsent, ObservedAt: time.Now().UTC()}
	result, err := (&PostgresRepository{}).ReconcileApproval(context.Background(), AdminApprovalControl{}, candidate, observation)
	if err != nil || result != ApprovalReconciliationBlockedEffectAuthority {
		t.Fatalf("result=%s err=%v", result, err)
	}
}
