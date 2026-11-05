package account

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestF14ChargebackPresenterRequiresNonzeroFrozenCaseIdentity(t *testing.T) {
	caseID, evidenceID, walletID, packageID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	item := ApprovalRequest{CommandType: "CHARGEBACK_FINANCIAL_REPAIR", PolicyVersion: 1, SnapshotSchemaVersion: 1, TargetResourceType: "CHARGEBACK_CASE", TargetResourceID: caseID.String()}
	summary := approvalV2Summary(item)
	command, ok := approvalV2Command(item).(approvalChargebackFinancialRepairSummary)
	if !ok || command.ChargebackCaseID != caseID || summary.DetailAvailability != "AVAILABLE" || summary.Target.ResourceType != "CHARGEBACK_CASE" || summary.Target.ResourceID != caseID.String() {
		t.Fatal("valid chargeback summary changed", summary)
	}
	proof := func(id uuid.UUID) []byte {
		encoded, err := json.Marshal(map[string]any{
			"normalized_command":         map[string]any{"chargeback_case_id": id, "payment_evidence_id": evidenceID, "expected_case_version": "7", "reason_code": "BASE_SHORTFALL_COMPENSATION"},
			"critical_resource_versions": map[string]any{"chargeback_case_version": "7", "facts_digest": strings.Repeat("ab", 32)},
			"financial_snapshot":         map[string]any{"wallet_id": walletID, "package_id": packageID, "base_shortfall_microcredits": "40", "component_digest": strings.Repeat("cd", 32)},
		})
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	valid, err := approvalChargebackFinancialDetail(approvalChargebackFinancialRepairSummary{ChargebackCaseID: caseID}, proof(caseID))
	if err != nil || valid.ChargebackCaseID != caseID || valid.PaymentEvidenceID != evidenceID || valid.ExpectedCaseVersion != "7" || valid.WalletID != walletID || valid.PackageID != packageID || valid.BaseShortfallMicrocredits != "40" || valid.ComponentDigest != strings.Repeat("cd", 32) || valid.FactsDigest != strings.Repeat("ab", 32) || valid.ReasonCode != "BASE_SHORTFALL_COMPENSATION" {
		t.Fatal("valid frozen chargeback detail changed", err)
	}
	for _, test := range []struct {
		name           string
		target, frozen uuid.UUID
	}{
		{"zero-target-and-zero-frozen", uuid.Nil, uuid.Nil},
		{"zero-frozen", caseID, uuid.Nil},
		{"zero-target", uuid.Nil, caseID},
		{"mismatched-frozen", caseID, uuid.New()},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.target == uuid.Nil {
				zero := item
				zero.TargetResourceID = test.target.String()
				summary := approvalV2Summary(zero)
				if approvalV2Command(zero) != nil || summary.DetailAvailability != "DETAIL_PRESENTATION_UNAVAILABLE" || summary.Target != (ApprovalV2Target{}) {
					t.Error("zero chargeback target was advertised as an available summary", summary)
				}
			}
			got, err := approvalChargebackFinancialDetail(approvalChargebackFinancialRepairSummary{ChargebackCaseID: test.target}, proof(test.frozen))
			if !errors.Is(err, ErrApprovalDetailPresentationUnavailable) || got != (approvalChargebackFinancialRepairSummary{}) {
				t.Error("invalid frozen chargeback identity was presented", err)
			}
		})
	}
}
