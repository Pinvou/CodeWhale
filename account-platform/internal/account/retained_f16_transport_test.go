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

const f16OwnerProjection = `{"command_type":"USAGE_SETTLEMENT_REPAIR","policy_version":1,"snapshot_schema_version":1,"target_resource_type":"USAGE_HOLD","target_resource_id":"00000000-0000-4000-8000-000000000001","normalized_command":{"usage_request_id":"00000000-0000-4000-8000-000000000002","hold_id":"00000000-0000-4000-8000-000000000001","evidence_kind":"LOSS_REVIEW","loss_review_id":"00000000-0000-4000-8000-000000000003","reason_code":"MISSING_USAGE_SETTLEMENT"},"critical_resource_versions":{"hold_updated_at":"2026-10-07T00:00:00.123456789Z","loss_review_updated_at":"2026-10-07T00:00:00.123456789Z","facts_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","policy_version":1},"financial_snapshot":{"expected_microcredits":9223372036854775807,"settled_microcredits":9007199254740993,"settlement_microcredits":9007199254740993,"overage_microcredits":9214364837600034814,"wallet_id":"00000000-0000-4000-8000-000000000004","wallet_version":9007199254740993,"allocations_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}`

func TestF16UsageSettlementOwnerPrecheckTransport(t *testing.T) {
	request := ApprovalPrecheckRequest{CommandType: "USAGE_SETTLEMENT_REPAIR", TargetResourceID: "00000000-0000-4000-8000-000000000001", UsageRequestID: "00000000-0000-4000-8000-000000000002", LossReviewID: "00000000-0000-4000-8000-000000000003", ReasonCode: "MISSING_USAGE_SETTLEMENT"}
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"exact-int64", f16OwnerProjection, true},
		{"foreign-evidence", strings.ReplaceAll(f16OwnerProjection, request.UsageRequestID, "00000000-0000-4000-8000-000000000004"), false},
		{"foreign-target", strings.Replace(f16OwnerProjection, request.TargetResourceID, "00000000-0000-4000-8000-000000000004", 1), false},
		{"wrong-reason", strings.Replace(f16OwnerProjection, "MISSING_USAGE_SETTLEMENT", "OTHER", 1), false},
		{"future-version", strings.Replace(f16OwnerProjection, `"policy_version":1`, `"policy_version":2`, 1), false},
		{"duplicate-key", strings.Replace(f16OwnerProjection, `"policy_version":1`, `"policy_version":1,"policy_version":1`, 1), false},
		{"trailing-object", f16OwnerProjection + ` {}`, false},
		{"foreign-review", strings.Replace(f16OwnerProjection, request.LossReviewID, "00000000-0000-4000-8000-000000000007", 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var got ApprovalPrecheckRequest
				if r.Method != "POST" || r.URL.Path != "/internal/v1/approval-command-prechecks" || r.Header.Get("Authorization") != "Bearer controlled-f16" || json.NewDecoder(r.Body).Decode(&got) != nil || got != request {
					t.Error("owner identity/facts changed")
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(tc.body))
			}))
			defer sv.Close()
			src, e := NewHTTPApprovalPrecheckSource(sv.URL, "controlled-f16")
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
	for _, change := range []func(*ApprovalPrecheckRequest){func(r *ApprovalPrecheckRequest) { r.UsageRequestID = "" }, func(r *ApprovalPrecheckRequest) { r.UsageRequestID = uuid.Nil.String() }, func(r *ApprovalPrecheckRequest) { r.PaymentOrderID = r.TargetResourceID }, func(r *ApprovalPrecheckRequest) { r.AmountMicrocredits = 1 }, func(r *ApprovalPrecheckRequest) { r.ExpectedVersion = 1 }, func(r *ApprovalPrecheckRequest) { r.LossReviewID = uuid.Nil.String() }} {
		bad := request
		change(&bad)
		if validApprovalPrecheckRequest(bad) {
			t.Fatal("illegal closed input accepted")
		}
	}
	// The new evidence field cannot leak into previously installed input shapes.
	if validApprovalPrecheckRequest(ApprovalPrecheckRequest{CommandType: "RECEIVABLE_WRITE_OFF", TargetResourceID: request.TargetResourceID, UsageRequestID: request.UsageRequestID, WriteOffMicrocredits: 1, ExpectedVersion: 1, ReasonCode: "APPROVED_RECEIVABLE_WRITE_OFF"}) {
		t.Fatal("foreign evidence accepted by old command")
	}
}
func TestF16UsageSettlementTypedEditAndResult(t *testing.T) {
	var owner struct {
		Normalized json.RawMessage `json:"normalized_command"`
		Critical   json.RawMessage `json:"critical_resource_versions"`
		Financial  json.RawMessage `json:"financial_snapshot"`
	}
	if json.Unmarshal([]byte(f16OwnerProjection), &owner) != nil {
		t.Fatal("fixture")
	}
	item := ApprovalRequest{CommandType: "USAGE_SETTLEMENT_REPAIR", PolicyVersion: 1, SnapshotSchemaVersion: 1, TargetResourceType: "USAGE_HOLD", TargetResourceID: "00000000-0000-4000-8000-000000000001", NormalizedCommand: owner.Normalized, CriticalResourceVersions: owner.Critical, FinancialSnapshot: owner.Financial}
	detail, e := approvalV2Detail(item, 1)
	if e != nil {
		t.Fatal(e)
	}
	got, ok := detail.Command.(approvalUsageSettlementRepairSummary)
	var wire map[string]any
	encoded, _ := json.Marshal(got)
	json.Unmarshal(encoded, &wire)
	if !ok || wire["usage_request_id"] != "00000000-0000-4000-8000-000000000002" || wire["loss_review_id"] != "00000000-0000-4000-8000-000000000003" || got.ExpectedMicrocredits != "9223372036854775807" || got.SettledMicrocredits != "9007199254740993" {
		t.Fatal("typed original intent lost", got)
	}
	if output := os.Getenv("F16_OWNER_PROJECTION_OUT"); output != "" {
		if e := os.WriteFile(output, []byte(f16OwnerProjection), 0600); e != nil {
			t.Fatal(e)
		}
	}
	if output := os.Getenv("F16_TYPED_DETAIL_OUT"); output != "" {
		encoded, _ := json.Marshal(detail)
		if e := os.WriteFile(output, encoded, 0600); e != nil {
			t.Fatal(e)
		}
	}
	t.Run("missing-evidence-fails-closed", func(t *testing.T) {
		item.NormalizedCommand = json.RawMessage(strings.Replace(string(owner.Normalized), `"loss_review_id":"00000000-0000-4000-8000-000000000003",`, "", 1))
		if _, e = approvalV2Detail(item, 1); e == nil {
			t.Fatal("missing original evidence accepted")
		}
	})
}
