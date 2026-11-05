package billing

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
	"strings"
	"testing"
	"time"
)

func TestPaymentFinancialRepairInstalledActivation(t *testing.T) {
	for _, state := range []string{"INACTIVE", "ACTIVE"} {
		t.Run(state, func(t *testing.T) {
			snapshot := validBillingApprovalActivationSnapshot(time.Now().UTC())
			for i := range snapshot.Activations {
				if snapshot.Activations[i].CommandType == paymentFinancialRepairCommand {
					snapshot.Activations[i].State = state
				}
			}
			if err := validateApprovalCommandActivationSnapshot(snapshot, time.Now().UTC()); err != nil {
				t.Fatalf("installed Payment catalog rejected %s: %v", state, err)
			}
		})
	}
}

func TestPaymentFinancialRepairFrozenDigestIsRequired(t *testing.T) {
	plan := PaymentFinancialRepairPlan{FactsDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	for _, tc := range []struct {
		name, digest string
		rejected     bool
	}{
		{"matching", plan.FactsDigest, false}, {"empty", "", true},
		{"changed", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := paymentFinancialRepairFrozenMismatch(plan, PaymentFinancialRepairFrozenFacts{FactsDigest: tc.digest})
			if (got != "") != tc.rejected {
				t.Fatalf("digest mismatch=%q rejected=%t", got, tc.rejected)
			}
		})
	}
}

// Controlled owner only: no SQL or provider effects inferred from these boundary cases.
type paymentPrecheckStub struct {
	billingApprovalPrecheckRepository
	input PaymentFinancialRepairCommandInput
	calls int
}

func (s *paymentPrecheckStub) PaymentFinancialRepairProofInput(_ context.Context, i PaymentFinancialRepairCommandInput) (approvalencoding.LegacyProofInput, error) {
	s.calls++
	s.input = i
	return approvalencoding.LegacyProofInput{CommandType: paymentFinancialRepairCommand, TargetResourceID: i.PaymentOrderID.String()}, nil
}
func TestPaymentFinancialRepairPrecheckClosedTransport(t *testing.T) {
	o, e := uuid.New(), uuid.New()
	body := `{"command_type":"PAYMENT_FINANCIAL_REPAIR","target_resource_id":"` + o.String() + `","payment_evidence_id":"` + e.String() + `","reason_code":"CONFIRMED_PAYMENT_NOT_CREDITED"}`
	request, err := decodeBillingApprovalPrecheckRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	owner := &paymentPrecheckStub{}
	result, err := precheckBillingApprovalCommand(context.Background(), owner, request)
	if err != nil || result.TargetResourceID != o.String() || owner.calls != 1 || owner.input.PaymentEvidenceID != e {
		t.Fatalf("exact owner mapping %+v %v", result, err)
	}
	for name, edit := range map[string]func(*billingApprovalPrecheckRequest){"empty-evidence": func(r *billingApprovalPrecheckRequest) { r.PaymentEvidenceID = "" }, "padded-id": func(r *billingApprovalPrecheckRequest) { r.TargetResourceID = " " + r.TargetResourceID }, "reason": func(r *billingApprovalPrecheckRequest) { r.ReasonCode += " " }, "foreign-receivable": func(r *billingApprovalPrecheckRequest) { r.WriteOffMicrocredits = 1 }, "foreign-ledger": func(r *billingApprovalPrecheckRequest) { r.WrongLedgerAccountID = uuid.NewString() }, "future": func(r *billingApprovalPrecheckRequest) { r.CommandType = "REFUND_EXCEPTION_REPAIR" }} {
		t.Run(name, func(t *testing.T) {
			bad := request
			edit(&bad)
			if _, err := precheckBillingApprovalCommand(context.Background(), owner, bad); !errors.Is(err, errBillingApprovalPrecheckUnavailable) || owner.calls != 1 {
				t.Fatalf("invalid reached owner: %v calls=%d", err, owner.calls)
			}
		})
	}
	for name, raw := range map[string]string{"duplicate": strings.TrimSuffix(body, "}") + `,"reason_code":"CONFIRMED_PAYMENT_NOT_CREDITED"}`, "unknown": strings.TrimSuffix(body, "}") + `,"facts_digest":"` + strings.Repeat("a", 64) + `"}`, "trailing": body + `{}`} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeBillingApprovalPrecheckRequest([]byte(raw)); err == nil {
				t.Fatal("noncontract JSON accepted")
			}
		})
	}
}

type paymentFactsStub struct {
	billingProofFacts
	plan PaymentFinancialRepairPlan
	err  error
}

func (s *paymentFactsStub) paymentFinancialRepairPlan(context.Context, PaymentFinancialRepairCommandInput) (PaymentFinancialRepairPlan, error) {
	return s.plan, s.err
}
func TestPaymentFinancialRepairOwnerProofProjection(t *testing.T) {
	p := PaymentFinancialRepairPlan{PolicyVersion: 1, OrderID: uuid.New(), EvidenceID: uuid.New(), WalletID: uuid.New(), OrderUpdatedAt: time.Now().UTC(), AmountFen: 9007199254740993, CreditMicrocredits: 9223372036854775807, WalletVersion: 9007199254740993, ProviderTransactionID: "durable-trade", FactsDigest: strings.Repeat("a", 64)}
	frozen := approvalencoding.LegacyProofInput{CommandType: paymentFinancialRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1, TargetResourceType: "PAYMENT_ORDER", TargetResourceID: p.OrderID.String(), NormalizedCommand: marshalRaw(map[string]any{"payment_order_id": p.OrderID, "payment_evidence_id": p.EvidenceID, "reason_code": paymentFinancialRepairReason})}
	owner := &paymentFactsStub{plan: p}
	current, reason, state, err := currentBillingProofInput(context.Background(), owner, frozen)
	if err != nil || reason != "" || state != p.ProviderTransactionID || !strings.Contains(string(current.FinancialSnapshot), "9223372036854775807") || !strings.Contains(string(current.FinancialSnapshot), "9007199254740993") {
		t.Fatalf("owner raw int64 projection %+v %s/%s %v", current, reason, state, err)
	}
	var critical struct {
		Policy int64 `json:"policy_version"`
	}
	if json.Unmarshal(current.CriticalResourceVersions, &critical) != nil || critical.Policy != p.PolicyVersion {
		t.Fatal("policy source lost")
	}
	for _, code := range []string{"", "OTHER", " " + paymentFinancialRepairReason, paymentFinancialRepairReason + " "} {
		bad := frozen
		bad.NormalizedCommand = marshalRaw(map[string]any{"payment_order_id": p.OrderID, "payment_evidence_id": p.EvidenceID, "reason_code": code})
		if _, _, _, err := currentBillingProofInput(context.Background(), owner, bad); !errors.Is(err, ErrApprovalSnapshotMismatch) {
			t.Fatalf("bad reason %q accepted %v", code, err)
		}
	}
	owner.plan.EvidenceID = uuid.New()
	if _, r, _, err := currentBillingProofInput(context.Background(), owner, frozen); err != nil || r != "SNAPSHOT_MISMATCH" {
		t.Fatalf("foreign evidence %s %v", r, err)
	}
	owner.err = ErrNotFound
	if _, r, _, err := currentBillingProofInput(context.Background(), owner, frozen); err != nil || r != "DOMAIN_INVARIANT_FAILED" {
		t.Fatalf("missing owner %s %v", r, err)
	}
	owner.err = errors.New("controlled owner unavailable")
	if _, _, _, err := currentBillingProofInput(context.Background(), owner, frozen); !errors.Is(err, owner.err) {
		t.Fatal("infra flattened", err)
	}
}
func TestPaymentFinancialRepairJSONSnapshotLossless(t *testing.T) {
	value, err := paymentRepairJSONSnapshot([]byte(`{"b":9007199254740993,"a":{"credit":9223372036854775807}}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(value)
	if err != nil || string(raw) != `{"a":{"credit":9223372036854775807},"b":9007199254740993}` {
		t.Fatalf("snapshot lost tokens %s %v", raw, err)
	}
	for _, bad := range []string{`[]`, `null`, `{} {}`, `{"a":`} {
		if _, err := paymentRepairJSONSnapshot([]byte(bad)); err == nil {
			t.Fatalf("invalid snapshot %s accepted", bad)
		}
	}
}
