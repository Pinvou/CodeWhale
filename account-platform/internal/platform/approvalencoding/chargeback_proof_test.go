package approvalencoding

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func chargebackProofFixture() LegacyProofInput {
	return LegacyProofInput{CommandType: "CHARGEBACK_FINANCIAL_REPAIR", PolicyVersion: 1, SnapshotSchemaVersion: 1, TargetResourceType: "CHARGEBACK_CASE", TargetResourceID: "00000000-0000-4000-8000-000000000001",
		NormalizedCommand:        json.RawMessage(`{"chargeback_case_id":"00000000-0000-4000-8000-000000000001","payment_evidence_id":"00000000-0000-4000-8000-000000000002","expected_case_version":9007199254740993,"reason_code":"BASE_SHORTFALL_COMPENSATION"}`),
		CriticalResourceVersions: json.RawMessage(`{"chargeback_case_version":9007199254740993,"facts_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","policy_version":1}`),
		FinancialSnapshot:        json.RawMessage(`{"base_shortfall_microcredits":9223372036854775807,"wallet_id":"00000000-0000-4000-8000-000000000003","wallet_version":9007199254740993,"package_id":"00000000-0000-4000-8000-000000000004","component_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","provider_chargeback_id":"controlled-provider-chargeback"}`)}
}
func TestChargebackProofClosedContract(t *testing.T) {
	t.Run("exact-wire-hashes-and-roundtrip", func(t *testing.T) {
		a, e := ConvertLegacyProofInput(chargebackProofFixture())
		if e != nil {
			t.Fatal(e)
		}
		for _, k := range []string{"expected_case_version", "chargeback_case_version", "wallet_version"} {
			if !bytes.Contains(a.ProofInputJSON, []byte(`"`+k+`":"9007199254740993"`)) {
				t.Fatal("lost exact decimal", k)
			}
		}
		if !bytes.Contains(a.ProofInputJSON, []byte(`"base_shortfall_microcredits":"9223372036854775807"`)) {
			t.Fatal("lost int64 maximum")
		}
		legacy, e := LegacyProofInputFromArtifacts(a)
		if e != nil {
			t.Fatal(e)
		}
		b, e := ConvertLegacyProofInput(legacy)
		if e != nil || a.CommandDigest != b.CommandDigest || a.SnapshotDigest != b.SnapshotDigest {
			t.Fatal("roundtrip changed freeze", e)
		}
		changed := chargebackProofFixture()
		changed.NormalizedCommand = bytes.Replace(changed.NormalizedCommand, []byte("00000000-0000-4000-8000-000000000002"), []byte("00000000-0000-4000-8000-000000000004"), 1)
		b, e = ConvertLegacyProofInput(changed)
		if e != nil || a.CommandDigest == b.CommandDigest || a.SnapshotDigest != b.SnapshotDigest {
			t.Fatal("original evidence hash binding", e)
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
			v.NormalizedCommand = bytes.Replace(v.NormalizedCommand, []byte("BASE_SHORTFALL_COMPENSATION"), []byte(" BASE_SHORTFALL_COMPENSATION"), 1)
		}},
		{"stale-version", func(v *LegacyProofInput) {
			v.CriticalResourceVersions = bytes.Replace(v.CriticalResourceVersions, []byte("9007199254740993"), []byte("9007199254740992"), 1)
		}},
		{"zero-version", func(v *LegacyProofInput) {
			v.NormalizedCommand = bytes.Replace(v.NormalizedCommand, []byte("9007199254740993"), []byte("0"), 1)
		}},
		{"foreign-field", func(v *LegacyProofInput) {
			v.NormalizedCommand = bytes.Replace(v.NormalizedCommand, []byte(`"reason_code":`), []byte(`"anomaly_case_id":"00000000-0000-4000-8000-000000000006","reason_code":`), 1)
		}},
		{"duplicate-key", func(v *LegacyProofInput) {
			v.FinancialSnapshot = bytes.Replace(v.FinancialSnapshot, []byte(`"wallet_version":`), []byte(`"wallet_version":1,"wallet_version":`), 1)
		}},
		{"overflow", func(v *LegacyProofInput) {
			v.FinancialSnapshot = bytes.Replace(v.FinancialSnapshot, []byte("9223372036854775807"), []byte("9223372036854775808"), 1)
		}},
		{"fraction", func(v *LegacyProofInput) {
			v.FinancialSnapshot = bytes.Replace(v.FinancialSnapshot, []byte("9223372036854775807"), []byte("1.5"), 1)
		}},
		{"zero-shortfall", func(v *LegacyProofInput) {
			v.FinancialSnapshot = bytes.Replace(v.FinancialSnapshot, []byte("9223372036854775807"), []byte("0"), 1)
		}},
		{"invalid-digest", func(v *LegacyProofInput) {
			v.CriticalResourceVersions = bytes.Replace(v.CriticalResourceVersions, []byte(strings.Repeat("a", 64)), []byte(strings.Repeat("z", 64)), 1)
		}},
		{"future-policy", func(v *LegacyProofInput) { v.PolicyVersion = 2 }},
		{"future-schema", func(v *LegacyProofInput) { v.SnapshotSchemaVersion = 2 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := chargebackProofFixture()
			tc.change(&v)
			if _, e := ConvertLegacyProofInput(v); e == nil {
				t.Fatal("invalid immutable Chargeback facts accepted")
			}
		})
	}
}
