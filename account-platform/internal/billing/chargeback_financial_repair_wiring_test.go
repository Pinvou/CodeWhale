package billing

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
)

func TestChargebackInstalledActivation(t *testing.T) {
	for _, state := range []string{"INACTIVE", "ACTIVE"} {
		t.Run(state, func(t *testing.T) {
			now := time.Now().UTC()
			s := validBillingApprovalActivationSnapshot(now)
			for i := range s.Activations {
				if s.Activations[i].CommandType == chargebackFinancialRepairCommand {
					s.Activations[i].State = state
				}
			}
			if err := validateApprovalCommandActivationSnapshot(s, now); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestChargebackFreshDigestRequired(t *testing.T) {
	p := ChargebackFinancialRepairPlan{FactsDigest: strings.Repeat("a", 64)}
	for _, c := range []struct {
		name, digest string
		deny         bool
	}{{"match", p.FactsDigest, false}, {"empty", "", true}, {"different", strings.Repeat("b", 64), true}} {
		t.Run(c.name, func(t *testing.T) {
			got := chargebackFinancialRepairFrozenMismatch(p, ChargebackFinancialRepairFrozenFacts{FactsDigest: c.digest})
			if (got != "") != c.deny {
				t.Fatalf("digest mismatch %q deny=%v", got, c.deny)
			}
		})
	}
}

type chargebackPrecheckStub struct {
	billingApprovalPrecheckRepository
	input ChargebackFinancialRepairCommandInput
	calls int
}

func (s *chargebackPrecheckStub) ChargebackFinancialRepairProofInput(_ context.Context, input ChargebackFinancialRepairCommandInput) (approvalencoding.LegacyProofInput, error) {
	s.input = input
	s.calls++
	return approvalencoding.LegacyProofInput{CommandType: chargebackFinancialRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1, TargetResourceType: "CHARGEBACK_CASE", TargetResourceID: input.ChargebackCaseID.String(), NormalizedCommand: marshalRaw(map[string]any{"chargeback_case_id": input.ChargebackCaseID, "payment_evidence_id": input.PaymentEvidenceID, "expected_case_version": int64(9007199254740993), "reason_code": chargebackFinancialRepairReason})}, nil
}

func TestChargebackPrecheckClosedTransport(t *testing.T) {
	caseID, evidence := uuid.New(), uuid.New()
	valid := `{"command_type":"CHARGEBACK_FINANCIAL_REPAIR","target_resource_id":"` + caseID.String() + `","payment_evidence_id":"` + evidence.String() + `","reason_code":"BASE_SHORTFALL_COMPENSATION"}`
	for _, c := range []struct {
		name, body string
		allow      bool
	}{
		{"exact-version", valid, true},
		{"unknown-version-field", strings.Replace(valid, `"reason_code":`, `"expected_case_version":9007199254740993,"reason_code":`, 1), false},
		{"missing-evidence", strings.Replace(valid, evidence.String(), "", 1), false},
		{"wrong-reason", strings.Replace(valid, "BASE_SHORTFALL_COMPENSATION", "OTHER", 1), false},
		{"foreign-anomaly", strings.Replace(valid, `"reason_code":`, `"anomaly_case_id":"`+uuid.NewString()+`","reason_code":`, 1), false},
		{"duplicate-field", strings.Replace(valid, `"command_type":`, `"command_type":"CHARGEBACK_FINANCIAL_REPAIR","command_type":`, 1), false},
		{"future", strings.Replace(valid, "CHARGEBACK_FINANCIAL_REPAIR", "RECONCILIATION_FINANCIAL_REPAIR", 1), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			request, err := decodeBillingApprovalPrecheckRequest([]byte(c.body))
			s := &chargebackPrecheckStub{}
			var result approvalencoding.LegacyProofInput
			if err == nil {
				result, err = precheckBillingApprovalCommand(context.Background(), s, request)
			}
			if (err == nil) != c.allow {
				t.Fatalf("allow=%v err=%v", c.allow, err)
			}
			if c.allow {
				var decoded map[string]json.RawMessage
				if result.TargetResourceID != caseID.String() || s.input.PaymentEvidenceID != evidence || json.Unmarshal(result.NormalizedCommand, &decoded) != nil || string(decoded["expected_case_version"]) != "9007199254740993" {
					t.Fatal("owner field drift")
				}
			}
		})
	}
}
