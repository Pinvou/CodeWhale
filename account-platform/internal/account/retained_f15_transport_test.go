package account

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

const f15OwnerProjection = `{"command_type":"RECONCILIATION_FINANCIAL_REPAIR","policy_version":1,"snapshot_schema_version":1,"target_resource_type":"RECONCILIATION_CASE","target_resource_id":"00000000-0000-4000-8000-000000000001","normalized_command":{"reconciliation_case_id":"00000000-0000-4000-8000-000000000001","payment_evidence_id":"00000000-0000-4000-8000-000000000002","business_date":"2026-10-07","resolution_code":"PROVIDER_REFUND_CONFIRMED"},"critical_resource_versions":{"run_id":"00000000-0000-4000-8000-000000000003","case_id":"00000000-0000-4000-8000-000000000001","provider_snapshot_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","facts_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","policy_version":1},"financial_snapshot":{"amount_fen":9007199254740993,"currency":"CNY","provider_transaction_id":"controlled-provider-transaction","ledger_effect":"NONE"}}`

func TestF15ReconciliationOwnerPrecheckTransport(t *testing.T) {
	request := ApprovalPrecheckRequest{CommandType: "RECONCILIATION_FINANCIAL_REPAIR", TargetResourceID: "00000000-0000-4000-8000-000000000001", PaymentEvidenceID: "00000000-0000-4000-8000-000000000002", BusinessDate: "2026-10-07", ReasonCode: "PROVIDER_REFUND_CONFIRMED"}
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"exact-int64", f15OwnerProjection, true},
		{"foreign-evidence", strings.ReplaceAll(f15OwnerProjection, request.PaymentEvidenceID, "00000000-0000-4000-8000-000000000004"), false},
		{"foreign-target", strings.Replace(f15OwnerProjection, request.TargetResourceID, "00000000-0000-4000-8000-000000000004", 1), false},
		{"wrong-reason", strings.Replace(f15OwnerProjection, "PROVIDER_REFUND_CONFIRMED", "OTHER", 1), false},
		{"future-version", strings.Replace(f15OwnerProjection, `"policy_version":1`, `"policy_version":2`, 1), false},
		{"duplicate-key", strings.Replace(f15OwnerProjection, `"policy_version":1`, `"policy_version":1,"policy_version":1`, 1), false},
		{"trailing-object", f15OwnerProjection + ` {}`, false},
		{"foreign-date", strings.Replace(f15OwnerProjection, "2026-10-07", "2026-10-08", 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var got ApprovalPrecheckRequest
				if r.Method != "POST" || r.URL.Path != "/internal/v1/approval-command-prechecks" || r.Header.Get("Authorization") != "Bearer controlled-f15" || json.NewDecoder(r.Body).Decode(&got) != nil || got != request {
					t.Error("owner identity/facts changed")
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(tc.body))
			}))
			defer sv.Close()
			src, e := NewHTTPApprovalPrecheckSource(sv.URL, "controlled-f15")
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
	for _, change := range []func(*ApprovalPrecheckRequest){func(r *ApprovalPrecheckRequest) { r.PaymentEvidenceID = "" }, func(r *ApprovalPrecheckRequest) { r.PaymentEvidenceID = uuid.Nil.String() }, func(r *ApprovalPrecheckRequest) { r.PaymentOrderID = r.TargetResourceID }, func(r *ApprovalPrecheckRequest) { r.AmountMicrocredits = 1 }, func(r *ApprovalPrecheckRequest) { r.ExpectedVersion = 1 }, func(r *ApprovalPrecheckRequest) { r.BusinessDate = "2026-02-30" }} {
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
func TestF15ReconciliationTypedEditAndResult(t *testing.T) {
	var owner struct {
		Normalized json.RawMessage `json:"normalized_command"`
		Critical   json.RawMessage `json:"critical_resource_versions"`
		Financial  json.RawMessage `json:"financial_snapshot"`
	}
	if json.Unmarshal([]byte(f15OwnerProjection), &owner) != nil {
		t.Fatal("fixture")
	}
	item := ApprovalRequest{CommandType: "RECONCILIATION_FINANCIAL_REPAIR", PolicyVersion: 1, SnapshotSchemaVersion: 1, TargetResourceType: "RECONCILIATION_CASE", TargetResourceID: "00000000-0000-4000-8000-000000000001", NormalizedCommand: owner.Normalized, CriticalResourceVersions: owner.Critical, FinancialSnapshot: owner.Financial}
	detail, e := approvalV2Detail(item, 1)
	if e != nil {
		t.Fatal(e)
	}
	got, ok := detail.Command.(approvalReconciliationFinancialRepairSummary)
	var wire map[string]any
	encoded, _ := json.Marshal(got)
	json.Unmarshal(encoded, &wire)
	if !ok || wire["payment_evidence_id"] != "00000000-0000-4000-8000-000000000002" || wire["business_date"] != "2026-10-07" || got.AmountFen != "9007199254740993" {
		t.Fatal("typed original intent lost", got)
	}
	if output := os.Getenv("F15_OWNER_PROJECTION_OUT"); output != "" {
		if e := os.WriteFile(output, []byte(f15OwnerProjection), 0600); e != nil {
			t.Fatal(e)
		}
	}
	if output := os.Getenv("F15_TYPED_DETAIL_OUT"); output != "" {
		encoded, _ := json.Marshal(detail)
		if e := os.WriteFile(output, encoded, 0600); e != nil {
			t.Fatal(e)
		}
	}
	t.Run("missing-evidence-fails-closed", func(t *testing.T) {
		item.NormalizedCommand = json.RawMessage(strings.Replace(string(owner.Normalized), `"payment_evidence_id":"00000000-0000-4000-8000-000000000002",`, "", 1))
		if _, e = approvalV2Detail(item, 1); e == nil {
			t.Fatal("missing original evidence accepted")
		}
	})
}
