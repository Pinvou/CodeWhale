package account

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const f12OwnerProjection = `{"command_type":"PAYMENT_FINANCIAL_REPAIR","policy_version":1,"snapshot_schema_version":1,"target_resource_type":"PAYMENT_ORDER","target_resource_id":"00000000-0000-4000-8000-000000000001","normalized_command":{"payment_order_id":"00000000-0000-4000-8000-000000000001","payment_evidence_id":"00000000-0000-4000-8000-000000000002","reason_code":"CONFIRMED_PAYMENT_NOT_CREDITED"},"critical_resource_versions":{"payment_order_updated_at":"2026-10-01T00:00:00Z","facts_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","policy_version":1},"financial_snapshot":{"amount_fen":9007199254740993,"credit_microcredits":9223372036854775807,"provider_transaction_id":"controlled-provider-tx","wallet_id":"00000000-0000-4000-8000-000000000003","wallet_version":9007199254740993,"applied_to_receivables":0}}`

func TestF12PaymentOwnerPrecheckTransport(t *testing.T) {
	request := ApprovalPrecheckRequest{CommandType: "PAYMENT_FINANCIAL_REPAIR", TargetResourceID: "00000000-0000-4000-8000-000000000001", PaymentEvidenceID: "00000000-0000-4000-8000-000000000002", ReasonCode: "CONFIRMED_PAYMENT_NOT_CREDITED"}
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"exact-int64", f12OwnerProjection, true},
		{"foreign-evidence", strings.ReplaceAll(f12OwnerProjection, request.PaymentEvidenceID, "00000000-0000-4000-8000-000000000004"), false},
		{"foreign-target", strings.Replace(f12OwnerProjection, request.TargetResourceID, "00000000-0000-4000-8000-000000000004", 1), false},
		{"wrong-reason", strings.Replace(f12OwnerProjection, "CONFIRMED_PAYMENT_NOT_CREDITED", "OTHER", 1), false},
		{"future-version", strings.Replace(f12OwnerProjection, `"policy_version":1`, `"policy_version":2`, 1), false},
		{"duplicate-key", strings.Replace(f12OwnerProjection, `"policy_version":1`, `"policy_version":1,"policy_version":1`, 1), false},
		{"trailing-object", f12OwnerProjection + ` {}`, false},
		{"excess-applied", strings.Replace(f12OwnerProjection, `"applied_to_receivables":0`, `"applied_to_receivables":9223372036854775808`, 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var got ApprovalPrecheckRequest
				if r.Method != "POST" || r.URL.Path != "/internal/v1/approval-command-prechecks" || r.Header.Get("Authorization") != "Bearer controlled-f12" || json.NewDecoder(r.Body).Decode(&got) != nil || got != request {
					t.Error("owner identity/facts changed")
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(tc.body))
			}))
			defer sv.Close()
			src, e := NewHTTPApprovalPrecheckSource(sv.URL, "controlled-f12")
			if e != nil {
				t.Fatal(e)
			}
			v, e := src.Precheck(context.Background(), request)
			if tc.valid {
				if e != nil || !strings.Contains(string(v.FinancialSnapshot), "9007199254740993") {
					t.Fatal("exact owner facts", e)
				}
			} else if e == nil {
				t.Fatal("untrusted ACK accepted")
			}
		})
	}
	for _, change := range []func(*ApprovalPrecheckRequest){func(r *ApprovalPrecheckRequest) { r.PaymentEvidenceID = "" }, func(r *ApprovalPrecheckRequest) { r.PaymentEvidenceID = uuid.Nil.String() }, func(r *ApprovalPrecheckRequest) { r.PaymentOrderID = r.TargetResourceID }, func(r *ApprovalPrecheckRequest) { r.AmountMicrocredits = 1 }, func(r *ApprovalPrecheckRequest) { r.ExpectedVersion = 1 }} {
		bad := request
		change(&bad)
		if validApprovalPrecheckRequest(bad) {
			t.Fatal("illegal closed input accepted")
		}
	}
	// The new evidence field cannot leak into previously installed input shapes.
	if validApprovalPrecheckRequest(ApprovalPrecheckRequest{CommandType: "RECEIVABLE_WRITE_OFF", TargetResourceID: request.TargetResourceID, PaymentEvidenceID: request.PaymentEvidenceID, WriteOffMicrocredits: 1, ExpectedVersion: 1, ReasonCode: "APPROVED_RECEIVABLE_WRITE_OFF"}) {
		t.Fatal("foreign evidence accepted by old command")
	}
}
func TestF12PaymentTypedEditAndResult(t *testing.T) {
	var owner struct {
		Normalized json.RawMessage `json:"normalized_command"`
		Critical   json.RawMessage `json:"critical_resource_versions"`
		Financial  json.RawMessage `json:"financial_snapshot"`
	}
	if json.Unmarshal([]byte(f12OwnerProjection), &owner) != nil {
		t.Fatal("fixture")
	}
	item := ApprovalRequest{CommandType: "PAYMENT_FINANCIAL_REPAIR", PolicyVersion: 1, SnapshotSchemaVersion: 1, TargetResourceType: "PAYMENT_ORDER", TargetResourceID: "00000000-0000-4000-8000-000000000001", NormalizedCommand: owner.Normalized, CriticalResourceVersions: owner.Critical, FinancialSnapshot: owner.Financial}
	detail, e := approvalV2Detail(item, 1)
	if e != nil {
		t.Fatal(e)
	}
	got, ok := detail.Command.(approvalPaymentFinancialRepairSummary)
	if !ok || got.PaymentEvidenceID.String() != "00000000-0000-4000-8000-000000000002" || got.WalletVersion != "9007199254740993" || got.CreditMicrocredits != "9223372036854775807" {
		t.Fatal("typed original intent lost", got)
	}
	t.Run("missing-evidence-fails-closed", func(t *testing.T) {
		item.NormalizedCommand = json.RawMessage(strings.Replace(string(owner.Normalized), `"payment_evidence_id":"00000000-0000-4000-8000-000000000002",`, "", 1))
		if _, e = approvalV2Detail(item, 1); e == nil {
			t.Fatal("missing original evidence accepted")
		}
	})
	raw := json.RawMessage(`{"repair_id":"00000000-0000-4000-8000-000000000001","payment_order_id":"00000000-0000-4000-8000-000000000001","payment_transaction_id":"00000000-0000-4000-8000-000000000002","entitlement_package_id":"00000000-0000-4000-8000-000000000003","journal_entry_id":"00000000-0000-4000-8000-000000000004","provider_transaction_id":"controlled-tx","credit_microcredits":9223372036854775807,"applied_to_receivables":0,"wallet_version_after":9007199254740993,"facts_digest":"` + strings.Repeat("a", 64) + `","status":"SUCCEEDED"}`)
	ledger := json.RawMessage(`{"journal_entry_id":"00000000-0000-4000-8000-000000000004"}`)
	t.Run("result-first-exact", func(t *testing.T) {
		if !validBillingRecoveryPayload("PAYMENT_FINANCIAL_REPAIR", raw, ledger) {
			t.Fatal("closed result rejected")
		}
	})
	t.Run("foreign-ledger-rejected", func(t *testing.T) {
		if validBillingRecoveryPayload("PAYMENT_FINANCIAL_REPAIR", raw, json.RawMessage(`{"journal_entry_id":"00000000-0000-4000-8000-000000000005"}`)) {
			t.Fatal("foreign ledger accepted")
		}
	})
}
