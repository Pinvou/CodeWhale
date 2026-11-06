package billing

import (
	"encoding/json"
	"testing"
)

func TestReconciliationIntegrityIdentityIgnoresConsumptionAndClock(t *testing.T) {
	var first, second reconciliationAuditRecord
	if err := json.Unmarshal([]byte(`{"packages":[{"id":"source","wallet_id":"wallet","points_total":100,"points_remaining":100,"status":"ACTIVE","updated_at":"old"}],"components":[{"id":"component","original_microcredits":100,"available_microcredits":100,"lot":{"id":"lot","original_microcredits":100,"remaining_microcredits":100}}]}`), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"packages":[{"id":"source","wallet_id":"wallet","points_total":100,"points_remaining":0,"status":"CONSUMED","updated_at":"new"}],"components":[{"id":"component","original_microcredits":100,"available_microcredits":0,"lot":{"id":"lot","original_microcredits":100,"remaining_microcredits":0}}]}`), &second); err != nil {
		t.Fatal(err)
	}
	if reconciliationIntegrityDigest(first) != reconciliationIntegrityDigest(second) {
		t.Fatal("consumption created a new integrity identity")
	}
	second["packages"] = json.RawMessage(`[{"id":"source","wallet_id":"wrong-wallet","points_total":100}]`)
	if reconciliationIntegrityDigest(first) == reconciliationIntegrityDigest(second) {
		t.Fatal("changed financial relation retained prior identity")
	}
}

func TestReconciliationIntegrityIdentityIncludesRefundAndReceivableSources(t *testing.T) {
	original := []byte(`{"source":{"id":"refund","payment_order_id":"order","payment_transaction_id":"transaction","entitlement_package_id":"package","amount_fen":50,"revoked_base_microcredits":500,"revoked_bonus_microcredits":20,"updated_at":"old","status":"RESOLVED"},"receivables":[{"id":"debt","wallet_id":"wallet","payment_order_id":"order","payment_transaction_id":"transaction","entitlement_package_id":"package","original_microcredits":500,"outstanding_microcredits":500}]}`)
	for _, scenario := range []struct {
		section, key string
		value        json.RawMessage
	}{
		{"source", "amount_fen", json.RawMessage(`60`)}, {"source", "revoked_base_microcredits", json.RawMessage(`600`)}, {"source", "revoked_bonus_microcredits", json.RawMessage(`21`)},
		{"source", "payment_order_id", json.RawMessage(`"other"`)}, {"source", "payment_transaction_id", json.RawMessage(`"other"`)}, {"source", "entitlement_package_id", json.RawMessage(`"other"`)},
		{"receivables", "payment_order_id", json.RawMessage(`"other"`)}, {"receivables", "payment_transaction_id", json.RawMessage(`"other"`)}, {"receivables", "entitlement_package_id", json.RawMessage(`"other"`)},
	} {
		t.Run(scenario.section+"/"+scenario.key, func(t *testing.T) {
			var before, after reconciliationAuditRecord
			_ = json.Unmarshal(original, &before)
			_ = json.Unmarshal(original, &after)
			if scenario.section == "source" {
				var row reconciliationAuditRecord
				_ = json.Unmarshal(after["source"], &row)
				row[scenario.key] = scenario.value
				after["source"], _ = json.Marshal(row)
			} else {
				rows := auditRecords(after["receivables"])
				rows[0][scenario.key] = scenario.value
				after["receivables"], _ = json.Marshal(rows)
			}
			if reconciliationIntegrityDigest(before) == reconciliationIntegrityDigest(after) {
				t.Fatal("changed stable financial source reused identity")
			}
		})
	}
	var before, after reconciliationAuditRecord
	_ = json.Unmarshal(original, &before)
	_ = json.Unmarshal(original, &after)
	var source reconciliationAuditRecord
	_ = json.Unmarshal(after["source"], &source)
	source["status"] = json.RawMessage(`"OTHER"`)
	source["updated_at"] = json.RawMessage(`"new"`)
	source["owner_id"] = json.RawMessage(`"owner"`)
	after["source"], _ = json.Marshal(source)
	rows := auditRecords(after["receivables"])
	rows[0]["outstanding_microcredits"] = json.RawMessage(`0`)
	after["receivables"], _ = json.Marshal(rows)
	if reconciliationIntegrityDigest(before) != reconciliationIntegrityDigest(after) {
		t.Fatal("mutable review or remaining values changed incident identity")
	}
}
