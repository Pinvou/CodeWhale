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

const ledgerPrecheckOwnerFixture = `{"command_type":"LEDGER_REPAIR","policy_version":1,"snapshot_schema_version":1,"target_resource_type":"LEDGER_POSTING","target_resource_id":"00000000-0000-4000-8000-000000000001","normalized_command":{"target_posting_id":"00000000-0000-4000-8000-000000000001","target_journal_entry_id":"00000000-0000-4000-8000-000000000002","wrong_ledger_account_id":"00000000-0000-4000-8000-000000000003","correct_ledger_account_id":"00000000-0000-4000-8000-000000000004","amount_microcredits":9223372036854775807,"reason_code":"ATTRIBUTION_TO_WRONG_PLATFORM_ACCOUNT"},"critical_resource_versions":{"target_posting_id":"00000000-0000-4000-8000-000000000001","target_journal_entry_id":"00000000-0000-4000-8000-000000000002","posting_set_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","entry_type":"USAGE","policy_version":1},"financial_snapshot":{"amount_microcredits":9223372036854775807,"wrong_ledger_account_id":"00000000-0000-4000-8000-000000000003","correct_ledger_account_id":"00000000-0000-4000-8000-000000000004","wrong_account_type":"ADJUSTMENT","correct_account_type":"USAGE_REVENUE","wallet_id":null}}`

func TestF10LedgerPrecheckOwnerTransport(t *testing.T) {
	request := ApprovalPrecheckRequest{CommandType: "LEDGER_REPAIR", TargetResourceID: "00000000-0000-4000-8000-000000000001", TargetJournalEntryID: "00000000-0000-4000-8000-000000000002", WrongLedgerAccountID: "00000000-0000-4000-8000-000000000003", CorrectLedgerAccountID: "00000000-0000-4000-8000-000000000004", AmountMicrocredits: 9223372036854775807, ReasonCode: "ATTRIBUTION_TO_WRONG_PLATFORM_ACCOUNT"}
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"exact-owner-int64", ledgerPrecheckOwnerFixture, true},
		{"foreign-correct-account", strings.Replace(ledgerPrecheckOwnerFixture, request.CorrectLedgerAccountID, "00000000-0000-4000-8000-000000000009", 1), false},
		{"changed-positive-amount", strings.ReplaceAll(ledgerPrecheckOwnerFixture, "9223372036854775807", "9223372036854775806"), false},
		{"obsolete-schema", strings.Replace(ledgerPrecheckOwnerFixture, `"snapshot_schema_version":1`, `"snapshot_schema_version":2`, 1), false},
		{"second-object", ledgerPrecheckOwnerFixture + ` {}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var got ApprovalPrecheckRequest
				if r.Method != "POST" || r.URL.Path != "/internal/v1/approval-command-prechecks" || r.Header.Get("Authorization") != "Bearer controlled-precheck" || json.NewDecoder(r.Body).Decode(&got) != nil || got != request {
					t.Error("authority or selected facts changed")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			source, err := NewHTTPApprovalPrecheckSource(server.URL, "controlled-precheck")
			if err != nil {
				t.Fatal(err)
			}
			projection, err := source.Precheck(context.Background(), request)
			if tc.valid {
				if err != nil || !strings.Contains(string(projection.FinancialSnapshot), "9223372036854775807") {
					t.Fatal("valid owner facts changed", err)
				}
			} else if err == nil {
				t.Fatal("foreign or incomplete owner response accepted")
			}
		})
	}
	t.Run("missing-fixed-reason-is-not-a-legal-owner-read", func(t *testing.T) {
		bad := request
		bad.ReasonCode = ""
		if validApprovalPrecheckRequest(bad) {
			t.Fatal("blank reason accepted")
		}
	})
}

func TestF10LedgerTypedPresenterAndRecovery(t *testing.T) {
	var owner struct {
		Normalized json.RawMessage `json:"normalized_command"`
		Critical   json.RawMessage `json:"critical_resource_versions"`
		Financial  json.RawMessage `json:"financial_snapshot"`
	}
	if json.Unmarshal([]byte(ledgerPrecheckOwnerFixture), &owner) != nil {
		t.Fatal("fixture")
	}
	item := ApprovalRequest{CommandType: "LEDGER_REPAIR", PolicyVersion: 1, SnapshotSchemaVersion: 1, TargetResourceType: "LEDGER_POSTING", TargetResourceID: "00000000-0000-4000-8000-000000000001", NormalizedCommand: owner.Normalized, CriticalResourceVersions: owner.Critical, FinancialSnapshot: owner.Financial}
	detail, err := approvalV2Detail(item, 1)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := detail.Command.(approvalLedgerRepairSummary)
	if !ok || got.AmountMicrocredits != "9223372036854775807" || got.WrongLedgerAccountID.String() != "00000000-0000-4000-8000-000000000003" || got.CorrectLedgerAccountID.String() != "00000000-0000-4000-8000-000000000004" || got.TargetJournalEntryID.String() != "00000000-0000-4000-8000-000000000002" {
		t.Fatal("typed EDIT lost frozen owner identity or exact amount")
	}
	id := uuid.NewString()
	raw := json.RawMessage(`{"case_id":"` + id + `","target_journal_entry_id":"` + id + `","target_posting_id":"` + id + `","wrong_ledger_account_id":"00000000-0000-4000-8000-000000000003","correct_ledger_account_id":"00000000-0000-4000-8000-000000000004","entry_type":"USAGE","amount_microcredits":9223372036854775807,"posting_set_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","journal_entry_id":"` + id + `","status":"SUCCEEDED"}`)
	ledger := json.RawMessage(`{"journal_entry_id":"` + id + `"}`)
	t.Run("exact-success", func(t *testing.T) {
		if !validBillingRecoveryPayload("LEDGER_REPAIR", raw, ledger) {
			t.Fatal("success lost")
		}
	})
	t.Run("foreign-ledger", func(t *testing.T) {
		if validBillingRecoveryPayload("LEDGER_REPAIR", raw, json.RawMessage(`{"journal_entry_id":"`+uuid.NewString()+`"}`)) {
			t.Fatal("foreign ledger accepted")
		}
	})
	t.Run("overflow", func(t *testing.T) {
		if validBillingRecoveryPayload("LEDGER_REPAIR", json.RawMessage(strings.Replace(string(raw), "9223372036854775807", "9223372036854775808", 1)), ledger) {
			t.Fatal("overflow accepted")
		}
	})
	t.Run("future-command", func(t *testing.T) {
		if validBillingRecoveryPayload("RECEIVABLE_WRITE_OFF", raw, ledger) {
			t.Fatal("future effect accepted")
		}
	})
}
