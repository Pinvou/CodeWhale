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

// TestChargebackFinancialRepairProofProjectionIsClosedAndActivationGated covers
// the GSA-32 proof-authority slice: the closed projection is recomputed by the
// owner domain, a rejected compensation is a closed DENY reason, and no proof
// can be issued while the command's activation row is INACTIVE.
func TestChargebackFinancialRepairProofProjectionIsClosedAndActivationGated(t *testing.T) {
	caseID, evidenceID, walletID, packageID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	frozen := approvalencoding.LegacyProofInput{
		CommandType: chargebackFinancialRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1,
		TargetResourceType: "CHARGEBACK_CASE", TargetResourceID: caseID.String(),
		NormalizedCommand: marshalRaw(map[string]any{
			"chargeback_case_id": caseID.String(), "payment_evidence_id": evidenceID.String(),
			"expected_case_version": 3, "reason_code": chargebackFinancialRepairReason,
		}),
	}
	plan := ChargebackFinancialRepairPlan{
		ChargebackCaseID: caseID, PolicyVersion: 1, CaseVersion: 3, EvidenceID: evidenceID, AnomalyCaseID: uuid.New(),
		AnomalyVersion: 2, AnomalyStatus: "PENDING", PackageID: packageID, WalletID: walletID, WalletVersion: 4,
		ProviderChargebackID: "cb-refund-1", BaseShortfallMicrocredits: 250,
		ComponentDigest: strings.Repeat("d", 64), FactsDigest: strings.Repeat("e", 64),
	}
	repository := &chargebackRepairProofFactsStub{chargebackRepair: plan}
	current, reason, ownerState, err := currentBillingProofInput(context.Background(), repository, frozen)
	if err != nil || reason != "" {
		t.Fatalf("chargeback repair proof reason=%s err=%v", reason, err)
	}
	if ownerState != "cb-refund-1" {
		t.Fatalf("unexpected owner state %q", ownerState)
	}
	var critical, financial map[string]any
	if err := json.Unmarshal(current.CriticalResourceVersions, &critical); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(current.FinancialSnapshot, &financial); err != nil {
		t.Fatal(err)
	}
	if critical["chargeback_case_version"] != float64(3) || critical["facts_digest"] != plan.FactsDigest ||
		critical["policy_version"] != float64(1) || len(critical) != 3 {
		t.Fatalf("unexpected critical projection %+v", critical)
	}
	if financial["base_shortfall_microcredits"] != float64(250) || financial["wallet_id"] != walletID.String() ||
		financial["wallet_version"] != float64(4) || financial["package_id"] != packageID.String() ||
		financial["component_digest"] != plan.ComponentDigest || financial["provider_chargeback_id"] != "cb-refund-1" ||
		len(financial) != 6 {
		t.Fatalf("unexpected financial projection %+v", financial)
	}

	rejected := &chargebackRepairProofFactsStub{chargebackRepairErr: ErrApprovalSnapshotMismatch}
	if _, reason, _, err := currentBillingProofInput(context.Background(), rejected, frozen); err != nil || reason != "DOMAIN_INVARIANT_FAILED" {
		t.Fatalf("rejected compensation reason=%s err=%v", reason, err)
	}
	mismatched := &chargebackRepairProofFactsStub{chargebackRepair: plan}
	mismatched.chargebackRepair.CaseVersion = 4
	if _, reason, _, err := currentBillingProofInput(context.Background(), mismatched, frozen); err != nil || reason != "SNAPSHOT_MISMATCH" {
		t.Fatalf("mismatched compensation reason=%s err=%v", reason, err)
	}
	foreignCase := &chargebackRepairProofFactsStub{chargebackRepair: plan}
	foreignCase.chargebackRepair.ChargebackCaseID = uuid.New()
	if _, reason, _, err := currentBillingProofInput(context.Background(), foreignCase, frozen); err != nil || reason != "SNAPSHOT_MISMATCH" {
		t.Fatalf("foreign case reason=%s err=%v", reason, err)
	}
	wrongReason := approvalencoding.LegacyProofInput{
		CommandType: chargebackFinancialRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1,
		NormalizedCommand: marshalRaw(map[string]any{
			"chargeback_case_id": caseID.String(), "payment_evidence_id": evidenceID.String(),
			"expected_case_version": 3, "reason_code": "SOMETHING_ELSE",
		}),
	}
	if _, _, _, err := currentBillingProofInput(context.Background(), repository, wrongReason); !errors.Is(err, ErrApprovalSnapshotMismatch) {
		t.Fatalf("wrong reason code err=%v", err)
	}

	now := time.Now().UTC()
	reader := &chargebackRepairApprovalProofStub{chargebackRepairProofFactsStub: repository, activation: validBillingApprovalActivationSnapshot(now)}
	service := NewService(reader)
	if _, err := service.billingApprovalProof(context.Background(), approvalencoding.ProofRequest{
		ApprovalRequestID: uuid.New(),
		Artifacts:         approvalencoding.ProofArtifacts{CommandType: chargebackFinancialRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1},
	}); !errors.Is(err, errBillingApprovalProofUnavailable) {
		t.Fatalf("inactive chargeback repair proof err=%v", err)
	}
}

// Fresh execution requires the exact frozen owner digest; an empty digest is
// not permission to execute against a different persisted owner revision.
func TestChargebackFinancialRepairFrozenFactsDigestIsRequired(t *testing.T) {
	plan := ChargebackFinancialRepairPlan{
		ChargebackCaseID: uuid.New(), CaseVersion: 2, BaseShortfallMicrocredits: 70,
		ComponentDigest: strings.Repeat("1", 64), WalletID: uuid.New(), WalletVersion: 5,
		PackageID: uuid.New(), FactsDigest: strings.Repeat("2", 64),
	}
	frozen := ChargebackFinancialRepairFrozenFacts{
		CaseVersion: plan.CaseVersion, BaseShortfallMicrocredits: plan.BaseShortfallMicrocredits,
		ComponentDigest: plan.ComponentDigest, WalletID: plan.WalletID, WalletVersion: plan.WalletVersion,
		PackageID: plan.PackageID,
	}
	if mismatch := chargebackFinancialRepairFrozenMismatch(plan, frozen); mismatch != "facts_digest" {
		t.Fatalf("empty digest accepted: %s", mismatch)
	}
	frozen.FactsDigest = strings.Repeat("3", 64)
	if mismatch := chargebackFinancialRepairFrozenMismatch(plan, frozen); mismatch != "facts_digest" {
		t.Fatalf("presented digest mismatch=%q", mismatch)
	}
	frozen.FactsDigest = plan.FactsDigest
	if mismatch := chargebackFinancialRepairFrozenMismatch(plan, frozen); mismatch != "" {
		t.Fatalf("matching digest rejected: %s", mismatch)
	}
	frozen.ComponentDigest = strings.Repeat("4", 64)
	if mismatch := chargebackFinancialRepairFrozenMismatch(plan, frozen); mismatch != "component_digest" {
		t.Fatalf("component digest mismatch=%q", mismatch)
	}
}

type chargebackRepairProofFactsStub struct {
	billingProofFactsStub
	chargebackRepair    ChargebackFinancialRepairPlan
	chargebackRepairErr error
}

func (s *chargebackRepairProofFactsStub) chargebackFinancialRepairPlan(context.Context, ChargebackFinancialRepairCommandInput) (ChargebackFinancialRepairPlan, error) {
	return s.chargebackRepair, s.chargebackRepairErr
}

type chargebackRepairApprovalProofStub struct {
	Repository
	*chargebackRepairProofFactsStub
	activation ApprovalCommandActivationSnapshot
}

func (s *chargebackRepairApprovalProofStub) ApprovalCommandActivations(context.Context) (ApprovalCommandActivationSnapshot, error) {
	return s.activation, nil
}
