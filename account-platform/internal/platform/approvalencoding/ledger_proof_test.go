package approvalencoding

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func ledgerProofFixture(t *testing.T) LegacyProofInput {
	t.Helper()
	posting := "00000000-0000-4000-8000-000000000001"
	journal := "00000000-0000-4000-8000-000000000002"
	wrong := "00000000-0000-4000-8000-000000000003"
	correct := "00000000-0000-4000-8000-000000000004"
	encode := func(value any) json.RawMessage {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	return LegacyProofInput{
		CommandType: "LEDGER_REPAIR", PolicyVersion: 1, SnapshotSchemaVersion: 1,
		TargetResourceType: "LEDGER_POSTING", TargetResourceID: posting,
		NormalizedCommand:        encode(map[string]any{"target_journal_entry_id": journal, "target_posting_id": posting, "wrong_ledger_account_id": wrong, "correct_ledger_account_id": correct, "amount_microcredits": int64(math.MaxInt64), "reason_code": "ATTRIBUTION_TO_WRONG_PLATFORM_ACCOUNT"}),
		CriticalResourceVersions: encode(map[string]any{"target_journal_entry_id": journal, "target_posting_id": posting, "posting_set_digest": strings.Repeat("a", 64), "entry_type": "USAGE", "policy_version": 1}),
		FinancialSnapshot:        encode(map[string]any{"amount_microcredits": int64(math.MaxInt64), "wrong_ledger_account_id": wrong, "correct_ledger_account_id": correct, "wrong_account_type": "ADJUSTMENT", "correct_account_type": "USAGE_REVENUE", "wallet_id": nil}),
	}
}

func TestLedgerProofClosedContract(t *testing.T) {
	t.Run("positive lossless maximum", func(t *testing.T) {
		proof, err := ConvertLegacyProofInput(ledgerProofFixture(t))
		if err != nil || proof.CommandType != "LEDGER_REPAIR" || proof.PolicyVersion != 1 || proof.SnapshotSchemaVersion != 1 || !bytes.Contains(proof.ProofInputJSON, []byte(`"amount_microcredits":"9223372036854775807"`)) {
			t.Fatalf("valid Ledger proof lost exact int64: %v", err)
		}
	})
	for _, reason := range []string{"", "OTHER_REASON", " ATTRIBUTION_TO_WRONG_PLATFORM_ACCOUNT", "ATTRIBUTION_TO_WRONG_PLATFORM_ACCOUNT "} {
		t.Run("invalid reason "+reason, func(t *testing.T) {
			input := ledgerProofFixture(t)
			var command map[string]json.RawMessage
			if err := json.Unmarshal(input.NormalizedCommand, &command); err != nil {
				t.Fatal(err)
			}
			command["reason_code"], _ = json.Marshal(reason)
			input.NormalizedCommand, _ = json.Marshal(command)
			if _, err := ConvertLegacyProofInput(input); err == nil {
				t.Fatal("noncontract reason accepted")
			}
		})
	}
	for _, test := range []struct {
		name   string
		mutate func(*LegacyProofInput)
	}{
		{"target differs", func(v *LegacyProofInput) { v.TargetResourceID = "00000000-0000-4000-8000-000000000009" }},
		{"journal differs", func(v *LegacyProofInput) {
			v.CriticalResourceVersions = bytes.Replace(v.CriticalResourceVersions, []byte("00000000-0000-4000-8000-000000000002"), []byte("00000000-0000-4000-8000-000000000009"), 1)
		}},
		{"amount differs", func(v *LegacyProofInput) {
			v.FinancialSnapshot = bytes.Replace(v.FinancialSnapshot, []byte("9223372036854775807"), []byte("9223372036854775806"), 1)
		}},
		{"foreign wallet", func(v *LegacyProofInput) {
			v.FinancialSnapshot = bytes.Replace(v.FinancialSnapshot, []byte(`"wallet_id":null`), []byte(`"wallet_id":"00000000-0000-4000-8000-000000000009"`), 1)
		}},
		{"foreign wrong account type", func(v *LegacyProofInput) {
			v.FinancialSnapshot = bytes.Replace(v.FinancialSnapshot, []byte(`"wrong_account_type":"ADJUSTMENT"`), []byte(`"wrong_account_type":"WALLET_LIABILITY"`), 1)
		}},
		{"foreign correct account type", func(v *LegacyProofInput) {
			v.FinancialSnapshot = bytes.Replace(v.FinancialSnapshot, []byte(`"correct_account_type":"USAGE_REVENUE"`), []byte(`"correct_account_type":"UNKNOWN"`), 1)
		}},
		{"future schema", func(v *LegacyProofInput) { v.SnapshotSchemaVersion = 2 }},
		{"future command", func(v *LegacyProofInput) { v.CommandType = "RECEIVABLE_WRITE_OFF" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := ledgerProofFixture(t)
			test.mutate(&input)
			if _, err := ConvertLegacyProofInput(input); err == nil {
				t.Fatal("forged Ledger document accepted")
			}
		})
	}
}
