package billing

import (
	"context"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
	"strings"
	"testing"
	"time"
)

func TestUsageRepairInstalledActivation(t *testing.T) {
	for _, state := range []string{"INACTIVE", "ACTIVE"} {
		t.Run(state, func(t *testing.T) {
			now := time.Now().UTC()
			s := validBillingApprovalActivationSnapshot(now)
			for i := range s.Activations {
				if s.Activations[i].CommandType == usageSettlementRepairCommand {
					s.Activations[i].State = state
				}
			}
			if err := validateApprovalCommandActivationSnapshot(s, now); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type usageRepairPrecheckStub struct {
	billingApprovalPrecheckRepository
	input UsageSettlementRepairCommandInput
}

func (s *usageRepairPrecheckStub) UsageSettlementRepairProofInput(_ context.Context, i UsageSettlementRepairCommandInput) (approvalencoding.LegacyProofInput, error) {
	s.input = i
	return approvalencoding.LegacyProofInput{CommandType: usageSettlementRepairCommand, TargetResourceType: "USAGE_HOLD", TargetResourceID: i.HoldID.String()}, nil
}
func TestUsageRepairPrecheckClosedTransport(t *testing.T) {
	h, u, l := uuid.New(), uuid.New(), uuid.New()
	valid := `{"command_type":"USAGE_SETTLEMENT_REPAIR","target_resource_id":"` + h.String() + `","usage_request_id":"` + u.String() + `","loss_review_id":"` + l.String() + `","reason_code":"MISSING_USAGE_SETTLEMENT"}`
	for _, c := range []struct {
		name, body string
		allow      bool
	}{
		{"exact-five", valid, true},
		{"missing-review", strings.Replace(valid, l.String(), "", 1), false},
		{"wrong-reason", strings.Replace(valid, "MISSING_USAGE_SETTLEMENT", "OTHER", 1), false},
		{"foreign-evidence", strings.Replace(valid, `"reason_code":`, `"payment_evidence_id":"`+uuid.NewString()+`","reason_code":`, 1), false},
		{"foreign-date", strings.Replace(valid, `"reason_code":`, `"business_date":"2026-10-10","reason_code":`, 1), false},
		{"duplicate", strings.Replace(valid, `"command_type":`, `"command_type":"USAGE_SETTLEMENT_REPAIR","command_type":`, 1), false},
		{"uninstalled", strings.Replace(valid, "USAGE_SETTLEMENT_REPAIR", "UNKNOWN_REPAIR", 1), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			request, err := decodeBillingApprovalPrecheckRequest([]byte(c.body))
			s := &usageRepairPrecheckStub{}
			var result approvalencoding.LegacyProofInput
			if err == nil {
				result, err = precheckBillingApprovalCommand(context.Background(), s, request)
			}
			if (err == nil) != c.allow {
				t.Fatalf("allow=%v %v", c.allow, err)
			}
			if c.allow && (s.input.HoldID != h || s.input.UsageRequestID != u || s.input.LossReviewID != l || result.TargetResourceID != h.String()) {
				t.Fatal("owner identity drift")
			}
		})
	}
	old := billingApprovalPrecheckRequest{CommandType: receivableWriteOffCommand, TargetResourceID: h.String(), WriteOffMicrocredits: 1, ExpectedVersion: 1, ReasonCode: receivableWriteOffReason, UsageRequestID: u.String()}
	if _, err := precheckBillingApprovalCommand(context.Background(), &usageRepairPrecheckStub{}, old); err == nil {
		t.Fatal("foreign new field accepted by prior command")
	}
}

func TestUsageRepairFreshDigestRequired(t *testing.T) {
	p := UsageSettlementRepairPlan{FactsDigest: strings.Repeat("a", 64)}
	for _, c := range []struct {
		name, digest string
		deny         bool
	}{{"match", p.FactsDigest, false}, {"empty", "", true}, {"different", strings.Repeat("b", 64), true}} {
		t.Run(c.name, func(t *testing.T) {
			got := usageSettlementRepairFrozenMismatch(p, UsageSettlementRepairFrozenFacts{FactsDigest: c.digest})
			if (got != "") != c.deny {
				t.Fatalf("digest mismatch %q deny=%v", got, c.deny)
			}
		})
	}
}
