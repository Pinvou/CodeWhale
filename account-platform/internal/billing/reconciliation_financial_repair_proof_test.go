package billing

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
)

// TestReconciliationFinancialRepairProofProjectionIsClosedAndActivationGated
// covers the GSA-34 proof-authority slice: the closed projection declares the
// frozen difference with an explicit zero ledger effect, a rejected closure is a
// closed DENY reason, and no proof can be issued while the command's activation
// row is INACTIVE.
func TestReconciliationFinancialRepairProofProjectionIsClosedAndActivationGated(t *testing.T) {
	caseID, runID, evidenceID := uuid.New(), uuid.New(), uuid.New()
	businessDate := time.Now().UTC().Format("2006-01-02")
	frozen := approvalencoding.LegacyProofInput{
		CommandType: reconciliationFinancialRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1,
		TargetResourceType: "RECONCILIATION_CASE", TargetResourceID: caseID.String(),
		NormalizedCommand: marshalRaw(map[string]any{
			"reconciliation_case_id": caseID.String(), "payment_evidence_id": evidenceID.String(),
			"business_date": businessDate, "resolution_code": reconciliationRepairResolutionCode,
		}),
	}
	plan := ReconciliationFinancialRepairPlan{
		CaseID: caseID, RunID: runID, PolicyVersion: 1, Channel: "ALIPAY", BusinessDate: businessDate,
		MerchantOrderNo: "unmatched-1", AmountFen: 1500, Currency: "CNY",
		ProviderTransactionID: "channel-tx-1", PaymentEvidenceID: evidenceID,
		ProviderSnapshotDigest: strings.Repeat("a", 64), FactsDigest: strings.Repeat("b", 64),
	}
	repository := &reconciliationRepairProofFactsStub{reconciliationRepair: plan}
	current, reason, ownerState, err := currentBillingProofInput(context.Background(), repository, frozen)
	if err != nil || reason != "" {
		t.Fatalf("reconciliation repair proof reason=%s err=%v", reason, err)
	}
	if ownerState != caseID.String()+":"+reconciliationRepairLedgerEffect {
		t.Fatalf("unexpected owner state %q", ownerState)
	}
	var critical, financial map[string]any
	if err := json.Unmarshal(current.CriticalResourceVersions, &critical); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(current.FinancialSnapshot, &financial); err != nil {
		t.Fatal(err)
	}
	if critical["run_id"] != runID.String() || critical["case_id"] != caseID.String() ||
		critical["provider_snapshot_digest"] != plan.ProviderSnapshotDigest ||
		critical["facts_digest"] != plan.FactsDigest || critical["policy_version"] != float64(1) || len(critical) != 5 {
		t.Fatalf("unexpected critical projection %+v", critical)
	}
	if financial["amount_fen"] != float64(1500) || financial["currency"] != "CNY" ||
		financial["provider_transaction_id"] != "channel-tx-1" || financial["ledger_effect"] != "NONE" ||
		len(financial) != 4 {
		t.Fatalf("unexpected financial projection %+v", financial)
	}

	rejected := &reconciliationRepairProofFactsStub{reconciliationRepairErr: ErrApprovalSnapshotMismatch}
	if _, reason, _, err := currentBillingProofInput(context.Background(), rejected, frozen); err != nil || reason != "DOMAIN_INVARIANT_FAILED" {
		t.Fatalf("rejected closure reason=%s err=%v", reason, err)
	}
	foreign := &reconciliationRepairProofFactsStub{reconciliationRepair: plan}
	foreign.reconciliationRepair.CaseID = uuid.New()
	if _, reason, _, err := currentBillingProofInput(context.Background(), foreign, frozen); err != nil || reason != "SNAPSHOT_MISMATCH" {
		t.Fatalf("foreign case reason=%s err=%v", reason, err)
	}
	wrongResolution := approvalencoding.LegacyProofInput{
		CommandType: reconciliationFinancialRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1,
		NormalizedCommand: marshalRaw(map[string]any{
			"reconciliation_case_id": caseID.String(), "payment_evidence_id": evidenceID.String(),
			"business_date": businessDate, "resolution_code": "SOMETHING_ELSE",
		}),
	}
	if _, _, _, err := currentBillingProofInput(context.Background(), repository, wrongResolution); !errors.Is(err, ErrApprovalSnapshotMismatch) {
		t.Fatalf("wrong resolution code err=%v", err)
	}

	now := time.Now().UTC()
	reader := &reconciliationRepairApprovalProofStub{reconciliationRepairProofFactsStub: repository, activation: validBillingApprovalActivationSnapshot(now)}
	service := NewService(reader)
	if _, err := service.billingApprovalProof(context.Background(), approvalencoding.ProofRequest{
		ApprovalRequestID: uuid.New(),
		Artifacts:         approvalencoding.ProofArtifacts{CommandType: reconciliationFinancialRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1},
	}); !errors.Is(err, errBillingApprovalProofUnavailable) {
		t.Fatalf("inactive reconciliation repair proof err=%v", err)
	}
}

// Fresh execution requires the frozen owner digest; matching receipt replay
// precedes fresh snapshot validation.
func TestReconciliationFinancialRepairFrozenFactsDigestIsRequired(t *testing.T) {
	plan := ReconciliationFinancialRepairPlan{
		CaseID: uuid.New(), RunID: uuid.New(), AmountFen: 900, Currency: "CNY",
		ProviderTransactionID: "channel-tx-2", ProviderSnapshotDigest: strings.Repeat("1", 64),
		FactsDigest: strings.Repeat("2", 64),
	}
	frozen := ReconciliationFinancialRepairFrozenFacts{RunID: plan.RunID, AmountFen: plan.AmountFen,
		Currency: plan.Currency, ProviderTransactionID: plan.ProviderTransactionID,
		ProviderSnapshotDigest: plan.ProviderSnapshotDigest}
	if mismatch := reconciliationFinancialRepairFrozenMismatch(plan, frozen); mismatch != "facts_digest" {
		t.Fatalf("empty digest accepted: %s", mismatch)
	}
	frozen.FactsDigest = strings.Repeat("3", 64)
	if mismatch := reconciliationFinancialRepairFrozenMismatch(plan, frozen); mismatch != "facts_digest" {
		t.Fatalf("presented digest mismatch=%q", mismatch)
	}
	frozen.FactsDigest = plan.FactsDigest
	if mismatch := reconciliationFinancialRepairFrozenMismatch(plan, frozen); mismatch != "" {
		t.Fatalf("matching digest rejected: %s", mismatch)
	}
	frozen.ProviderSnapshotDigest = strings.Repeat("4", 64)
	if mismatch := reconciliationFinancialRepairFrozenMismatch(plan, frozen); mismatch != "provider_snapshot_digest" {
		t.Fatalf("provider snapshot digest mismatch=%q", mismatch)
	}
}

type reconciliationRepairProofFactsStub struct {
	billingProofFactsStub
	reconciliationRepair    ReconciliationFinancialRepairPlan
	reconciliationRepairErr error
}

func (s *reconciliationRepairProofFactsStub) reconciliationFinancialRepairPlan(context.Context, ReconciliationFinancialRepairCommandInput) (ReconciliationFinancialRepairPlan, error) {
	return s.reconciliationRepair, s.reconciliationRepairErr
}

type reconciliationRepairApprovalProofStub struct {
	Repository
	*reconciliationRepairProofFactsStub
	activation ApprovalCommandActivationSnapshot
}

func (s *reconciliationRepairApprovalProofStub) ApprovalCommandActivations(context.Context) (ApprovalCommandActivationSnapshot, error) {
	return s.activation, nil
}
