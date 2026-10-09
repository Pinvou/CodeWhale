package billing

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
	"strings"
	"testing"
	"time"
)

// Only a verified terminal success can produce refund evidence. A nil
// transaction makes any attempted write for an interim/failure result fail the
// test, rather than depending on a permissive database substitute.
func TestRefundSuccessEvidenceRejectsNonSuccessBeforeTransaction(t *testing.T) {
	order := PaymentOrder{ID: uuid.New()}
	refund := Refund{ID: uuid.New(), PaymentOrderID: order.ID, PaymentTransactionID: uuid.New(), MerchantRefundNo: "original-refund", AmountFen: 17}
	for _, status := range []string{"PENDING", "PROCESSING", "FAILED"} {
		t.Run(status, func(t *testing.T) {
			result := ProviderRefund{Status: status, ChannelRefundID: "original-provider-refund", MerchantRefundNo: refund.MerchantRefundNo, AmountFen: refund.AmountFen}
			id, err := (&PaymentCoordinator{}).insertRefundSuccessEvidence(context.Background(), nil, order, refund, result, time.Now().UTC())
			if id != uuid.Nil || !errors.Is(err, ErrConflict) {
				t.Fatalf("non-success evidence accepted: %s %v", id, err)
			}
		})
	}
}

func TestRefundExceptionInstalledActivation(t *testing.T) {
	for _, state := range []string{"INACTIVE", "ACTIVE"} {
		t.Run(state, func(t *testing.T) {
			s := validBillingApprovalActivationSnapshot(time.Now().UTC())
			for i := range s.Activations {
				if s.Activations[i].CommandType == refundExceptionRepairCommand {
					s.Activations[i].State = state
				}
			}
			if err := validateApprovalCommandActivationSnapshot(s, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestRefundExceptionFreshDigestRequired(t *testing.T) {
	p := RefundExceptionRepairPlan{FactsDigest: strings.Repeat("a", 64)}
	for _, c := range []struct {
		name, digest string
		deny         bool
	}{{"match", p.FactsDigest, false}, {"empty", "", true}, {"different", strings.Repeat("b", 64), true}} {
		t.Run(c.name, func(t *testing.T) {
			got := refundExceptionRepairFrozenMismatch(p, RefundExceptionRepairFrozenFacts{FactsDigest: c.digest})
			if (got != "") != c.deny {
				t.Fatalf("digest mismatch %q deny %v", got, c.deny)
			}
		})
	}
}

type refundPrecheckStub struct {
	billingApprovalPrecheckRepository
	input RefundExceptionRepairCommandInput
	calls int
}

func (s *refundPrecheckStub) RefundExceptionRepairProofInput(_ context.Context, in RefundExceptionRepairCommandInput) (approvalencoding.LegacyProofInput, error) {
	s.input = in
	s.calls++
	return approvalencoding.LegacyProofInput{CommandType: refundExceptionRepairCommand, TargetResourceID: in.RefundID.String()}, nil
}
func TestRefundExceptionPrecheckClosedTransport(t *testing.T) {
	r, e, a := uuid.New(), uuid.New(), uuid.New()
	body := `{"command_type":"REFUND_EXCEPTION_REPAIR","target_resource_id":"` + r.String() + `","payment_evidence_id":"` + e.String() + `","anomaly_case_id":"` + a.String() + `","reason_code":"PROVIDER_REFUNDED_LOCAL_REVERSAL_MISSING"}`
	request, err := decodeBillingApprovalPrecheckRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	owner := &refundPrecheckStub{}
	out, err := precheckBillingApprovalCommand(context.Background(), owner, request)
	if err != nil || out.TargetResourceID != r.String() || owner.input.PaymentEvidenceID != e || owner.input.AnomalyCaseID != a || owner.calls != 1 {
		t.Fatalf("projection %+v calls %d err %v", out, owner.calls, err)
	}
	for name, raw := range map[string]string{"missing-anomaly": strings.Replace(body, a.String(), "", 1), "wrong-reason": strings.Replace(body, refundExceptionRepairReason, "UNKNOWN", 1), "duplicate": strings.Replace(body, `"command_type":`, `"command_type":"REFUND_EXCEPTION_REPAIR","command_type":`, 1), "future": strings.Replace(body, refundExceptionRepairCommand, "CHARGEBACK_FINANCIAL_REPAIR", 1), "foreign-field": strings.Replace(body, `"reason_code":`, `"write_off_microcredits":1,"reason_code":`, 1)} {
		t.Run(name, func(t *testing.T) {
			in, err := decodeBillingApprovalPrecheckRequest([]byte(raw))
			if err == nil {
				_, err = precheckBillingApprovalCommand(context.Background(), owner, in)
			}
			if err == nil {
				t.Fatal("invalid request accepted")
			}
		})
	}
}
