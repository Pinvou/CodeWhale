package approvalencoding

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func receivableProofFixture() LegacyProofInput {
	return LegacyProofInput{
		CommandType: "RECEIVABLE_WRITE_OFF", PolicyVersion: 1, SnapshotSchemaVersion: 1,
		TargetResourceType: "RECEIVABLE", TargetResourceID: "00000000-0000-4000-8000-000000000001",
		NormalizedCommand:        json.RawMessage(`{"receivable_id":"00000000-0000-4000-8000-000000000001","write_off_microcredits":9223372036854775807,"expected_receivable_version":9007199254740993,"reason_code":"APPROVED_RECEIVABLE_WRITE_OFF"}`),
		CriticalResourceVersions: json.RawMessage(`{"receivable_version":9007199254740993,"facts_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","policy_version":1}`),
		FinancialSnapshot:        json.RawMessage(`{"write_off_microcredits":9223372036854775807,"outstanding_microcredits":9223372036854775807,"settled_microcredits":0,"original_microcredits":9223372036854775807,"source_type":"USAGE_OVERAGE","source_reference":"usage:controlled","wallet_receivable_microcredits":9223372036854775807}`),
	}
}

func TestReceivableProofClosedContract(t *testing.T) {
	t.Run("exact-int64-and-bound-request", func(t *testing.T) {
		a, err := ConvertLegacyProofInput(receivableProofFixture())
		if err != nil || !bytes.Contains(a.ProofInputJSON, []byte(`"write_off_microcredits":"9223372036854775807"`)) || !bytes.Contains(a.ProofInputJSON, []byte(`"expected_receivable_version":"9007199254740993"`)) {
			t.Fatalf("lost the original amount/version: %v", err)
		}
	})
	for _, tc := range []struct {
		name   string
		change func(*LegacyProofInput)
	}{
		{"foreign-target", func(v *LegacyProofInput) { v.TargetResourceID = "00000000-0000-4000-8000-000000000002" }},
		{"changed-version", func(v *LegacyProofInput) {
			v.CriticalResourceVersions = bytes.Replace(v.CriticalResourceVersions, []byte("9007199254740993"), []byte("9007199254740992"), 1)
		}},
		{"changed-amount", func(v *LegacyProofInput) {
			v.FinancialSnapshot = bytes.Replace(v.FinancialSnapshot, []byte("9223372036854775807"), []byte("9223372036854775806"), 1)
		}},
		{"overflow", func(v *LegacyProofInput) {
			v.NormalizedCommand = bytes.Replace(v.NormalizedCommand, []byte("9223372036854775807"), []byte("9223372036854775808"), 1)
		}},
		{"negative-amount", func(v *LegacyProofInput) {
			v.NormalizedCommand = bytes.Replace(v.NormalizedCommand, []byte("9223372036854775807"), []byte("-1"), 1)
		}},
		{"foreign-source", func(v *LegacyProofInput) {
			v.FinancialSnapshot = bytes.Replace(v.FinancialSnapshot, []byte("USAGE_OVERAGE"), []byte("PAYMENT"), 1)
		}},
		{"bad-sum", func(v *LegacyProofInput) {
			v.FinancialSnapshot = bytes.Replace(v.FinancialSnapshot, []byte(`"settled_microcredits":0`), []byte(`"settled_microcredits":1`), 1)
		}},
		{"insufficient-wallet", func(v *LegacyProofInput) {
			v.FinancialSnapshot = bytes.Replace(v.FinancialSnapshot, []byte(`"wallet_receivable_microcredits":9223372036854775807`), []byte(`"wallet_receivable_microcredits":1`), 1)
		}},
		{"invalid-digest", func(v *LegacyProofInput) {
			v.CriticalResourceVersions = bytes.Replace(v.CriticalResourceVersions, []byte(strings.Repeat("a", 64)), []byte(strings.Repeat("z", 64)), 1)
		}},
		{"future-schema", func(v *LegacyProofInput) { v.SnapshotSchemaVersion = 2 }},
		{"spaced-reason", func(v *LegacyProofInput) {
			v.NormalizedCommand = bytes.Replace(v.NormalizedCommand, []byte("APPROVED_RECEIVABLE_WRITE_OFF"), []byte(" APPROVED_RECEIVABLE_WRITE_OFF"), 1)
		}},
		{"duplicate-key", func(v *LegacyProofInput) {
			v.NormalizedCommand = bytes.Replace(v.NormalizedCommand, []byte(`"reason_code":`), []byte(`"write_off_microcredits":1,"reason_code":`), 1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := receivableProofFixture()
			tc.change(&v)
			if _, err := ConvertLegacyProofInput(v); err == nil {
				t.Fatal("foreign or invalid frozen facts accepted")
			}
		})
	}
}
