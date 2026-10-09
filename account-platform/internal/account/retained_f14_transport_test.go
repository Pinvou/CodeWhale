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

const f14OwnerProjection = `{"command_type":"CHARGEBACK_FINANCIAL_REPAIR","policy_version":1,"snapshot_schema_version":1,"target_resource_type":"CHARGEBACK_CASE","target_resource_id":"00000000-0000-4000-8000-000000000001","normalized_command":{"chargeback_case_id":"00000000-0000-4000-8000-000000000001","payment_evidence_id":"00000000-0000-4000-8000-000000000002","expected_case_version":9007199254740993,"reason_code":"BASE_SHORTFALL_COMPENSATION"},"critical_resource_versions":{"chargeback_case_version":9007199254740993,"facts_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","policy_version":1},"financial_snapshot":{"base_shortfall_microcredits":9223372036854775807,"wallet_id":"00000000-0000-4000-8000-000000000003","wallet_version":9007199254740993,"package_id":"00000000-0000-4000-8000-000000000004","component_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","provider_chargeback_id":"controlled-provider-chargeback"}}`

func TestF14ChargebackOwnerPrecheckTransport(t *testing.T) {
	request := ApprovalPrecheckRequest{CommandType: "CHARGEBACK_FINANCIAL_REPAIR", TargetResourceID: "00000000-0000-4000-8000-000000000001", PaymentEvidenceID: "00000000-0000-4000-8000-000000000002", ReasonCode: "BASE_SHORTFALL_COMPENSATION"}
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"exact-int64", f14OwnerProjection, true},
		{"foreign-evidence", strings.ReplaceAll(f14OwnerProjection, request.PaymentEvidenceID, "00000000-0000-4000-8000-000000000004"), false},
		{"foreign-target", strings.Replace(f14OwnerProjection, request.TargetResourceID, "00000000-0000-4000-8000-000000000004", 1), false},
		{"wrong-reason", strings.Replace(f14OwnerProjection, "BASE_SHORTFALL_COMPENSATION", "OTHER", 1), false},
		{"future-version", strings.Replace(f14OwnerProjection, `"policy_version":1`, `"policy_version":2`, 1), false},
		{"duplicate-key", strings.Replace(f14OwnerProjection, `"policy_version":1`, `"policy_version":1,"policy_version":1`, 1), false},
		{"trailing-object", f14OwnerProjection + ` {}`, false},
		{"zero-version", strings.Replace(f14OwnerProjection, `"expected_case_version":9007199254740993`, `"expected_case_version":0`, 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var got ApprovalPrecheckRequest
				if r.Method != "POST" || r.URL.Path != "/internal/v1/approval-command-prechecks" || r.Header.Get("Authorization") != "Bearer controlled-f14" || json.NewDecoder(r.Body).Decode(&got) != nil || got != request {
					t.Error("owner identity/facts changed")
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(tc.body))
			}))
			defer sv.Close()
			src, e := NewHTTPApprovalPrecheckSource(sv.URL, "controlled-f14")
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
func TestF14ChargebackTypedEditAndResult(t *testing.T) {
	var owner struct {
		Normalized json.RawMessage `json:"normalized_command"`
		Critical   json.RawMessage `json:"critical_resource_versions"`
		Financial  json.RawMessage `json:"financial_snapshot"`
	}
	if json.Unmarshal([]byte(f14OwnerProjection), &owner) != nil {
		t.Fatal("fixture")
	}
	item := ApprovalRequest{CommandType: "CHARGEBACK_FINANCIAL_REPAIR", PolicyVersion: 1, SnapshotSchemaVersion: 1, TargetResourceType: "CHARGEBACK_CASE", TargetResourceID: "00000000-0000-4000-8000-000000000001", NormalizedCommand: owner.Normalized, CriticalResourceVersions: owner.Critical, FinancialSnapshot: owner.Financial}
	detail, e := approvalV2Detail(item, 1)
	if e != nil {
		t.Fatal(e)
	}
	got, ok := detail.Command.(approvalChargebackFinancialRepairSummary)
	var wire map[string]any
	encoded, _ := json.Marshal(got)
	json.Unmarshal(encoded, &wire)
	if !ok || wire["payment_evidence_id"] != "00000000-0000-4000-8000-000000000002" || wire["expected_case_version"] != "9007199254740993" || got.BaseShortfallMicrocredits != "9223372036854775807" {
		t.Fatal("typed original intent lost", got)
	}
	if output := os.Getenv("F14_OWNER_PROJECTION_OUT"); output != "" {
		if e := os.WriteFile(output, []byte(f14OwnerProjection), 0600); e != nil {
			t.Fatal(e)
		}
	}
	if output := os.Getenv("F14_TYPED_DETAIL_OUT"); output != "" {
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
