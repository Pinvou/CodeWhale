package billing

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestUnknownDateOrphanCaseIdentityScope(t *testing.T) {
	id := uuid.New()
	snapshot, _ := json.Marshal(map[string]any{"payment_date_verification_required": true, "payment_order": map[string]any{"id": id, "status": "PAID", "fulfillment_status": "CREDITED", "paid_at": nil, "amount_fen": 50, "currency": "CNY"}})
	difference := ReconciliationDifference{PaymentOrderID: &id, MerchantOrderNo: "order", CaseType: "PAYMENT_TRANSACTION_MISSING", InternalEntryType: "PAYMENT", InternalReference: "ORPHAN_PAYMENT_ORDER:" + id.String(), InternalSnapshot: snapshot, InternalAmountFen: 50, InternalCurrency: "CNY"}
	run := ReconciliationRun{ProviderID: "provider", ProviderVersion: "v1", Channel: "channel", BusinessDate: "2026-11-01"}
	key, fingerprint := reconciliationCaseIdentity(run, difference), unknownDateOrphanFingerprint(run, difference)
	otherDate := run
	otherDate.BusinessDate = "2026-11-02"
	if reconciliationCaseIdentity(otherDate, difference) != key || unknownDateOrphanFingerprint(otherDate, difference) != fingerprint || reconciliationDifferenceDigest(otherDate, difference) == reconciliationDifferenceDigest(run, difference) {
		t.Fatal("case identity, acceptance fingerprint and immutable observation lost their distinct date semantics")
	}
	changed := difference
	changed.InternalAmountFen++
	if reconciliationCaseIdentity(run, changed) != key || unknownDateOrphanFingerprint(run, changed) == fingerprint {
		t.Fatal("changed financial facts must retain the investigation but require a new manual acceptance")
	}
	for _, mutate := range []func(*ReconciliationRun){func(r *ReconciliationRun) { r.ProviderID += "2" }, func(r *ReconciliationRun) { r.ProviderVersion += "2" }, func(r *ReconciliationRun) { r.Channel += "2" }} {
		other := run
		mutate(&other)
		if reconciliationCaseIdentity(other, difference) == key {
			t.Fatal("provider generation/channel scope collapsed")
		}
	}
	for _, raw := range []string{
		`{"payment_date_verification_required":true}`,
		`{"payment_date_verification_required":true,"payment_order":{"paid_at":null}}`,
		string(snapshot[:len(snapshot)-1]) + `,"payment_transaction":{}}`,
	} {
		other := difference
		other.InternalSnapshot = json.RawMessage(raw)
		if unknownDateOrphanDifference(other) || reconciliationCaseIdentity(run, other) != reconciliationDifferenceDigest(run, other) {
			t.Fatal("unproven snapshot entered the special identity path")
		}
	}
	other := difference
	other.CaseType = "PAYMENT_LEDGER_MISSING"
	if unknownDateOrphanDifference(other) || reconciliationCaseIdentity(run, other) != reconciliationDifferenceDigest(run, other) {
		t.Fatal("another case type entered the special identity path")
	}
}
