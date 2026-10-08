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
