package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const f11OwnerProjection = `{"command_type":"RECEIVABLE_WRITE_OFF","policy_version":1,"snapshot_schema_version":1,"target_resource_type":"RECEIVABLE","target_resource_id":"00000000-0000-4000-8000-000000000001","normalized_command":{"receivable_id":"00000000-0000-4000-8000-000000000001","write_off_microcredits":9223372036854775807,"expected_receivable_version":9007199254740993,"reason_code":"APPROVED_RECEIVABLE_WRITE_OFF"},"critical_resource_versions":{"receivable_version":9007199254740993,"facts_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","policy_version":1},"financial_snapshot":{"write_off_microcredits":9223372036854775807,"outstanding_microcredits":9223372036854775807,"settled_microcredits":0,"original_microcredits":9223372036854775807,"source_type":"USAGE_OVERAGE","source_reference":"usage:controlled","wallet_receivable_microcredits":9223372036854775807}}`

func TestF11ReceivableOwnerPrecheckTransport(t *testing.T) {
	request := ApprovalPrecheckRequest{CommandType: "RECEIVABLE_WRITE_OFF", TargetResourceID: "00000000-0000-4000-8000-000000000001", WriteOffMicrocredits: 9223372036854775807, ExpectedVersion: 9007199254740993, ReasonCode: "APPROVED_RECEIVABLE_WRITE_OFF"}
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"exact-int64-original-version", f11OwnerProjection, true}, {"changed-amount", strings.ReplaceAll(f11OwnerProjection, "9223372036854775807", "9223372036854775806"), false}, {"changed-original-version", strings.ReplaceAll(f11OwnerProjection, "9007199254740993", "9007199254740992"), false}, {"foreign-target", strings.Replace(f11OwnerProjection, request.TargetResourceID, "00000000-0000-4000-8000-000000000002", 1), false}, {"future-schema", strings.Replace(f11OwnerProjection, `"snapshot_schema_version":1`, `"snapshot_schema_version":2`, 1), false}, {"duplicate-key", strings.Replace(f11OwnerProjection, `"policy_version":1`, `"policy_version":1,"policy_version":1`, 1), false}, {"second-object", f11OwnerProjection + ` {}`, false}} {
		t.Run(tc.name, func(t *testing.T) {
			sv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var got ApprovalPrecheckRequest
				if r.Method != "POST" || r.URL.Path != "/internal/v1/approval-command-prechecks" || r.Header.Get("Authorization") != "Bearer controlled-f11" || json.NewDecoder(r.Body).Decode(&got) != nil || got != request {
					t.Error("selected identity/facts changed")
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(tc.body))
			}))
			defer sv.Close()
			src, e := NewHTTPApprovalPrecheckSource(sv.URL, "controlled-f11")
			if e != nil {
				t.Fatal(e)
			}
			v, e := src.Precheck(context.Background(), request)
			if tc.valid {
				if e != nil || !strings.Contains(string(v.NormalizedCommand), "9007199254740993") {
					t.Fatal("valid owner projection", e)
				}
			} else if e == nil {
				t.Fatal("foreign/noncanonical owner ACK accepted")
			}
		})
	}
	for _, change := range []func(*ApprovalPrecheckRequest){func(r *ApprovalPrecheckRequest) { r.ReasonCode = "" }, func(r *ApprovalPrecheckRequest) { r.AmountMicrocredits = 1 }, func(r *ApprovalPrecheckRequest) { r.PaymentOrderID = r.TargetResourceID }, func(r *ApprovalPrecheckRequest) { r.ExpectedVersion = 0 }} {
		bad := request
		change(&bad)
		if validApprovalPrecheckRequest(bad) {
			t.Fatal("illegal selected tuple accepted")
		}
	}
}
func TestF11ReceivableTypedEditAndRecovery(t *testing.T) {
	var owner struct {
		Normalized json.RawMessage `json:"normalized_command"`
		Critical   json.RawMessage `json:"critical_resource_versions"`
		Financial  json.RawMessage `json:"financial_snapshot"`
	}
	if json.Unmarshal([]byte(f11OwnerProjection), &owner) != nil {
		t.Fatal("fixture")
	}
	detail, e := approvalV2Detail(ApprovalRequest{CommandType: "RECEIVABLE_WRITE_OFF", PolicyVersion: 1, SnapshotSchemaVersion: 1, TargetResourceType: "RECEIVABLE", TargetResourceID: "00000000-0000-4000-8000-000000000001", NormalizedCommand: owner.Normalized, CriticalResourceVersions: owner.Critical, FinancialSnapshot: owner.Financial}, 1)
	if e != nil {
		t.Fatal(e)
	}
	got, ok := detail.Command.(approvalReceivableWriteOffSummary)
	if !ok || got.ExpectedReceivableVersion != "9007199254740993" || got.WriteOffMicrocredits != "9223372036854775807" {
		t.Fatal("typed EDIT lost original intent", got)
	}
	raw := json.RawMessage(`{"case_id":"00000000-0000-4000-8000-000000000001","receivable_id":"00000000-0000-4000-8000-000000000001","wallet_id":"00000000-0000-4000-8000-000000000002","amount_microcredits":9223372036854775807,"receivable_version_before":1,"receivable_version_after":2,"status_after":"WRITTEN_OFF","facts_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","journal_entry_id":"00000000-0000-4000-8000-000000000003","allocation_id":"00000000-0000-4000-8000-000000000004","status":"SUCCEEDED"}`)
	ledger := json.RawMessage(`{"journal_entry_id":"00000000-0000-4000-8000-000000000003"}`)
	t.Run("exact-result-first", func(t *testing.T) {
		if !validBillingRecoveryPayload("RECEIVABLE_WRITE_OFF", raw, ledger) {
			t.Fatal("valid result rejected")
		}
	})
	for name, bad := range map[string]json.RawMessage{"overflow": json.RawMessage(strings.Replace(string(raw), "9223372036854775807", "9223372036854775808", 1)), "wrong-successor-version": json.RawMessage(strings.Replace(string(raw), `"receivable_version_after":2`, `"receivable_version_after":3`, 1)), "unknown-terminal": json.RawMessage(strings.Replace(string(raw), "WRITTEN_OFF", "UNKNOWN", 1))} {
		t.Run(name, func(t *testing.T) {
			if validBillingRecoveryPayload("RECEIVABLE_WRITE_OFF", bad, ledger) {
				t.Fatal("invalid result accepted")
			}
		})
	}
	t.Run("foreign-ledger", func(t *testing.T) {
		if validBillingRecoveryPayload("RECEIVABLE_WRITE_OFF", raw, json.RawMessage(`{"journal_entry_id":"00000000-0000-4000-8000-000000000005"}`)) {
			t.Fatal("foreign ledger accepted")
		}
	})
}
