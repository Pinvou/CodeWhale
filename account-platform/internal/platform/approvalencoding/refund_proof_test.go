package approvalencoding

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func refundProofFixture() LegacyProofInput {
	return LegacyProofInput{CommandType: "REFUND_EXCEPTION_REPAIR", PolicyVersion: 1, SnapshotSchemaVersion: 1, TargetResourceType: "REFUND", TargetResourceID: "00000000-0000-4000-8000-000000000001",
		NormalizedCommand:        json.RawMessage(`{"refund_id":"00000000-0000-4000-8000-000000000001","payment_evidence_id":"00000000-0000-4000-8000-000000000002","anomaly_case_id":"00000000-0000-4000-8000-000000000006","reason_code":"PROVIDER_REFUNDED_LOCAL_REVERSAL_MISSING"}`),
		CriticalResourceVersions: json.RawMessage(`{"refund_updated_at":"2026-10-01T00:00:00Z","anomaly_version":9007199254740993,"facts_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","policy_version":1}`),
		FinancialSnapshot:        json.RawMessage(`{"amount_fen":9007199254740993,"credit_microcredits":9223372036854775807,"provider_refund_id":"controlled-provider-tx","wallet_id":"00000000-0000-4000-8000-000000000003","wallet_version":9007199254740993,"revoked_microcredits":9223372036854775806,"shortfall_microcredits":1,"package_id":"00000000-0000-4000-8000-000000000004"}`)}
}

func TestRefundProofClosedContract(t *testing.T) {
	t.Run("exact-wire-and-hash", func(t *testing.T) {
		a, e := ConvertLegacyProofInput(refundProofFixture())
		if e != nil {
			t.Fatal(e)
		}
		for _, key := range []string{"amount_fen", "wallet_version", "anomaly_version"} {
			if !bytes.Contains(a.ProofInputJSON, []byte(`"`+key+`":"9007199254740993"`)) {
				t.Fatal("lost exact decimal", key)
			}
		}
		if !bytes.Contains(a.ProofInputJSON, []byte(`"credit_microcredits":"9223372036854775807"`)) {
			t.Fatal("lost int64 maximum")
		}
		altered := refundProofFixture()
		altered.NormalizedCommand = bytes.Replace(altered.NormalizedCommand, []byte("00000000-0000-4000-8000-000000000002"), []byte("00000000-0000-4000-8000-000000000004"), 1)
		b, e := ConvertLegacyProofInput(altered)
		// CommandDigest binds original evidence selection. SnapshotDigest binds
		// the independently frozen critical/financial documents, which did not change.
		if e != nil || a.CommandDigest == b.CommandDigest || a.SnapshotDigest != b.SnapshotDigest {
			t.Fatal("immutable evidence not bound to hashes", e)
		}
	})
	for _, tc := range []struct {
		name   string
		change func(*LegacyProofInput)
	}{
		{"wrong-target", func(v *LegacyProofInput) { v.TargetResourceID = "00000000-0000-4000-8000-000000000004" }},
		{"nil-evidence", func(v *LegacyProofInput) {
			v.NormalizedCommand = bytes.Replace(v.NormalizedCommand, []byte("00000000-0000-4000-8000-000000000002"), []byte("00000000-0000-0000-0000-000000000000"), 1)
		}},
		{"spaced-reason", func(v *LegacyProofInput) {
			v.NormalizedCommand = bytes.Replace(v.NormalizedCommand, []byte("PROVIDER_REFUNDED_LOCAL_REVERSAL_MISSING"), []byte(" PROVIDER_REFUNDED_LOCAL_REVERSAL_MISSING"), 1)
		}},
		{"foreign-command-field", func(v *LegacyProofInput) {
			v.NormalizedCommand = bytes.Replace(v.NormalizedCommand, []byte(`"reason_code":`), []byte(`"expected_receivable_version":1,"reason_code":`), 1)
		}},
		{"duplicate-key", func(v *LegacyProofInput) {
			v.FinancialSnapshot = bytes.Replace(v.FinancialSnapshot, []byte(`"wallet_version":`), []byte(`"wallet_version":1,"wallet_version":`), 1)
		}},
		{"overflow", func(v *LegacyProofInput) {
			v.FinancialSnapshot = bytes.ReplaceAll(v.FinancialSnapshot, []byte("9223372036854775807"), []byte("9223372036854775808"))
		}},
		{"fraction", func(v *LegacyProofInput) {
			v.FinancialSnapshot = bytes.Replace(v.FinancialSnapshot, []byte(`"amount_fen":9007199254740993`), []byte(`"amount_fen":1.5`), 1)
		}},
		{"negative-shortfall", func(v *LegacyProofInput) {
			v.FinancialSnapshot = bytes.Replace(v.FinancialSnapshot, []byte(`"shortfall_microcredits":1`), []byte(`"shortfall_microcredits":-1`), 1)
		}},
		{"excess-revoked", func(v *LegacyProofInput) {
			v.FinancialSnapshot = bytes.Replace(v.FinancialSnapshot, []byte(`"credit_microcredits":9223372036854775807`), []byte(`"credit_microcredits":1`), 1)
		}},
		{"invalid-digest", func(v *LegacyProofInput) {
			v.CriticalResourceVersions = bytes.Replace(v.CriticalResourceVersions, []byte(strings.Repeat("a", 64)), []byte(strings.Repeat("z", 64)), 1)
		}},
		{"future-policy", func(v *LegacyProofInput) { v.PolicyVersion = 2 }},
		{"future-schema", func(v *LegacyProofInput) { v.SnapshotSchemaVersion = 2 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := refundProofFixture()
			tc.change(&v)
			if _, e := ConvertLegacyProofInput(v); e == nil {
				t.Fatal("invalid immutable refund facts accepted")
			}
		})
	}
}
