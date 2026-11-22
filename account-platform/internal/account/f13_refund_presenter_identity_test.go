package account

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestF13RefundPresenterRequiresNonzeroFrozenRefundIdentity(t *testing.T) {
	refundID, evidenceID, anomalyID, walletID, packageID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	item := ApprovalRequest{CommandType: "REFUND_EXCEPTION_REPAIR", PolicyVersion: 1, SnapshotSchemaVersion: 1, TargetResourceType: "REFUND", TargetResourceID: refundID.String()}
	summary := approvalV2Summary(item)
	command, ok := approvalV2Command(item).(approvalRefundExceptionRepairSummary)
	if !ok || command.RefundID != refundID || summary.DetailAvailability != "AVAILABLE" || summary.Target.ResourceType != "REFUND" || summary.Target.ResourceID != refundID.String() {
		t.Fatal("valid refund summary changed", summary)
	}
	proof := func(id uuid.UUID) []byte {
		encoded, err := json.Marshal(map[string]any{
			"normalized_command":         map[string]any{"refund_id": id, "payment_evidence_id": evidenceID, "anomaly_case_id": anomalyID, "reason_code": "PROVIDER_REFUNDED_LOCAL_REVERSAL_MISSING"},
			"critical_resource_versions": map[string]any{"facts_digest": strings.Repeat("ab", 32)},
			"financial_snapshot":         map[string]any{"wallet_id": walletID, "package_id": packageID, "amount_fen": "100", "credit_microcredits": "100", "revoked_microcredits": "60", "shortfall_microcredits": "40"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	valid, err := approvalRefundFinancialDetail(approvalRefundExceptionRepairSummary{RefundID: refundID}, proof(refundID))
	if err != nil || valid.RefundID != refundID || valid.PaymentEvidenceID != evidenceID || valid.AnomalyCaseID != anomalyID || valid.WalletID != walletID || valid.PackageID != packageID || valid.AmountFen != "100" || valid.CreditMicrocredits != "100" || valid.RevokedMicrocredits != "60" || valid.ShortfallMicrocredits != "40" || valid.FactsDigest != strings.Repeat("ab", 32) || valid.ReasonCode != "PROVIDER_REFUNDED_LOCAL_REVERSAL_MISSING" {
		t.Fatal("valid frozen refund detail changed", err)
	}
	for _, test := range []struct {
		name           string
		target, frozen uuid.UUID
	}{
		{"zero-target-and-zero-frozen", uuid.Nil, uuid.Nil},
		{"zero-frozen", refundID, uuid.Nil},
		{"zero-target", uuid.Nil, refundID},
		{"mismatched-frozen", refundID, uuid.New()},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.target == uuid.Nil {
				zero := item
				zero.TargetResourceID = test.target.String()
				summary := approvalV2Summary(zero)
				if approvalV2Command(zero) != nil || summary.DetailAvailability != "DETAIL_PRESENTATION_UNAVAILABLE" || summary.Target != (ApprovalV2Target{}) {
					t.Fatal("zero refund target was advertised as an available summary", summary)
				}
			}
			got, err := approvalRefundFinancialDetail(approvalRefundExceptionRepairSummary{RefundID: test.target}, proof(test.frozen))
			if !errors.Is(err, ErrApprovalDetailPresentationUnavailable) || got != (approvalRefundExceptionRepairSummary{}) {
				t.Fatal("invalid frozen refund identity was presented", err)
			}
		})
	}
}
