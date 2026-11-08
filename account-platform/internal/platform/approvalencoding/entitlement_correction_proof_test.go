package approvalencoding

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEntitlementCorrectionFirstDeliveryClosedProof(t *testing.T) {
	packageID := "00000000-0000-4000-8000-000000000001"
	orderID := "00000000-0000-4000-8000-000000000002"
	encode := func(value any) json.RawMessage {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	input := LegacyProofInput{
		CommandType: "ENTITLEMENT_COMPONENT_ADJUSTMENT", PolicyVersion: 2, SnapshotSchemaVersion: 2,
		TargetResourceType: "ENTITLEMENT_PACKAGE", TargetResourceID: packageID,
		NormalizedCommand: encode(map[string]any{"package_id": packageID, "payment_order_id": orderID, "reason_code": "PRODUCT_SPLIT_CORRECTION"}),
		CriticalResourceVersions: encode(map[string]any{
			"package_id": packageID, "wallet_id": "00000000-0000-4000-8000-000000000003", "wallet_version": int64(7),
			"payment_order_id": orderID, "payment_transaction_id": "00000000-0000-4000-8000-000000000004",
			"source_component_id": "00000000-0000-4000-8000-000000000005", "source_lot_id": "00000000-0000-4000-8000-000000000006",
			"target_component_id": nil, "target_lot_id": nil, "source_type": "BASE", "target_type": "BONUS",
			"amount_microcredits": int64(9007199254740993), "source_evidence_digest": strings.Repeat("a", 64), "facts_digest": strings.Repeat("b", 64), "policy_version": int64(2),
		}),
		FinancialSnapshot: encode(map[string]any{"package_total_microcredits": int64(9223372036854775807), "source_original_microcredits": int64(9223372036854775807), "target_original_microcredits": int64(0), "effective_total_microcredits": int64(9223372036854775807)}),
	}
	t.Run("full-int64-numeric-owner-projection", func(t *testing.T) {
		artifact, err := ConvertLegacyProofInput(input)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(artifact.ProofInputJSON), `"amount_microcredits":"9007199254740993"`) || !strings.Contains(string(artifact.ProofInputJSON), `"effective_total_microcredits":"9223372036854775807"`) {
			t.Fatal("exact integer facts lost in proof")
		}
		restored, err := LegacyProofInputFromArtifacts(artifact)
		if err != nil {
			t.Fatal(err)
		}
		again, err := ConvertLegacyProofInput(restored)
		if err != nil || again.CommandDigest != artifact.CommandDigest || again.SnapshotDigest != artifact.SnapshotDigest {
			t.Fatalf("proof roundtrip err=%v", err)
		}
	})
	t.Run("v1-history-is-not-current-v2-execution", func(t *testing.T) {
		wrong := input
		wrong.PolicyVersion = 1
		wrong.SnapshotSchemaVersion = 1
		if _, err := ConvertLegacyProofInput(wrong); err == nil {
			t.Fatal("obsolete proof activated")
		}
	})
	t.Run("foreign-order-does-not-bind", func(t *testing.T) {
		wrong := input
		wrong.NormalizedCommand = encode(map[string]any{"package_id": packageID, "payment_order_id": "00000000-0000-4000-8000-000000000007", "reason_code": "PRODUCT_SPLIT_CORRECTION"})
		if _, err := ConvertLegacyProofInput(wrong); err == nil {
			t.Fatal("foreign order bound to owner facts")
		}
	})
	t.Run("later-financial-command-not-installed", func(t *testing.T) {
		wrong := input
		wrong.CommandType = "LEDGER_REPAIR"
		if _, err := ConvertLegacyProofInput(wrong); err == nil {
			t.Fatal("later command allowed")
		}
	})
}
