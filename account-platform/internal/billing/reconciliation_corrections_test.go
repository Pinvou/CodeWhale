package billing

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func correctionAuditFixture(t *testing.T) (reconciliationAuditRequest, reconciliationAuditRecord) {
	t.Helper()
	id := func() string { return uuid.NewString() }
	wallet, owner, order, transaction, pkg := id(), id(), id(), id(), id()
	sourceID, sourceLot, originalBaseID, originalBaseLot, targetID, targetLot := id(), id(), id(), id(), id(), id()
	correction, approval, adjustment, recharge, walletAccount := id(), id(), id(), id(), id()
	reference := "payment-transaction:" + transaction
	encode := func(v any) json.RawMessage {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	component := func(componentID, lotID, kind, ref string, amount, out int) map[string]any {
		return map[string]any{"id": componentID, "package_id": pkg, "credit_lot_id": lotID, "component_type": kind, "original_microcredits": amount, "reclassified_out_microcredits": out, "credited_at": "2026-10-01T00:00:00Z", "expires_at": "2026-11-01T00:00:00Z", "lot": map[string]any{"id": lotID, "wallet_id": wallet, "source_type": "RECHARGE", "source_reference": ref, "original_microcredits": amount}}
	}
	account := map[string]any{"id": walletAccount, "wallet_id": wallet, "code": "wallet:" + wallet, "account_type": "WALLET_LIABILITY"}
	post := func(journal, lot string, amount int) map[string]any {
		return map[string]any{"journal_entry_id": journal, "ledger_account_id": walletAccount, "credit_lot_id": lot, "delta_microcredits": amount, "account": account}
	}
	payload := reconciliationAuditRecord{
		"order":               encode(map[string]any{"id": order, "requester_id": owner, "wallet_id": wallet, "amount_fen": 100, "currency": "CNY", "credit_microcredits": 100, "product_snapshot": Product{BasePoints: 80, BonusPoints: 20}}),
		"transaction":         encode(map[string]any{"id": transaction, "payment_order_id": order, "amount_fen": 100, "currency": "CNY"}),
		"packages":            encode([]any{map[string]any{"id": pkg, "user_id": owner, "wallet_id": wallet, "payment_order_id": order, "source_payment_transaction_id": transaction, "source_type": "PAYMENT", "points_total": 100, "amount_fen": 100}}),
		"components":          encode([]any{component(originalBaseID, originalBaseLot, "BASE", reference, 60, 0), component(sourceID, sourceLot, "BONUS", reference+":bonus", 40, 20), component(targetID, targetLot, "BASE", reference+":correction:"+correction, 20, 0)}),
		"corrections":         encode([]any{map[string]any{"id": correction, "approval_request_id": approval, "authorization_event_id": id(), "approval_epoch": 2, "execution_key_digest": strings.Repeat("a", 64), "package_id": pkg, "wallet_id": wallet, "payment_order_id": order, "payment_transaction_id": transaction, "source_component_id": sourceID, "source_lot_id": sourceLot, "target_component_id": targetID, "target_lot_id": targetLot, "source_type": "BONUS", "target_type": "BASE", "amount_microcredits": 20, "reason_code": "ORIGINAL_SPLIT_MISMATCH", "source_evidence_digest": strings.Repeat("b", 64), "facts_digest": strings.Repeat("c", 64), "journal_entry_id": adjustment}}),
		"correction_journals": encode([]any{map[string]any{"id": adjustment, "entry_type": "ADJUSTMENT", "business_reference": "approval-execution:" + approval}}),
		"correction_postings": encode([]any{post(adjustment, sourceLot, -20), post(adjustment, targetLot, 20)}),
		"journals":            encode([]any{map[string]any{"id": recharge, "entry_type": "RECHARGE", "business_reference": reference}}),
		"postings":            encode([]any{post(recharge, originalBaseLot, 60), post(recharge, sourceLot, 40), map[string]any{"journal_entry_id": recharge, "ledger_account_id": paymentClearingAccount.String(), "delta_microcredits": -100, "account": map[string]any{"id": paymentClearingAccount.String(), "account_type": "PAYMENT_CLEARING", "code": "platform:payment-clearing"}}}),
	}
	return reconciliationAuditRequest{Order: order, Transaction: transaction, References: []string{reference}}, payload
}

func TestReconciliationCorrectionKeepsOriginalFundingAndEffectiveEntitlements(t *testing.T) {
	request, payload := correctionAuditFixture(t)
	fact := ReconciliationInternalFact{EntryType: "PAYMENT"}
	auditReconciliationFact(&fact, request, payload)
	if !fact.LedgerValid || fact.IntegrityCaseType != "" {
		t.Fatalf("valid immutable correction rejected: %+v", fact)
	}
	packages := auditRecords(payload["packages"])
	amounts, ok := auditCorrectionAmounts(request, payload, auditString(packages[0], "wallet_id"), auditString(packages[0], "id"))
	if !ok || amounts.total.Cmp(big.NewInt(100)) != 0 || len(amounts.funding) != 2 || len(amounts.effective) != 3 || amounts.byKind["BASE"].Cmp(big.NewInt(80)) != 0 || amounts.byKind["BONUS"].Cmp(big.NewInt(20)) != 0 {
		t.Fatal("funding and corrected rights were conflated")
	}
	ceiling, ok := auditComponentProportion(amounts.pieces["BASE"], big.NewInt(1), big.NewInt(100), true)
	if !ok || ceiling.Cmp(big.NewInt(2)) != 0 {
		t.Fatal("component rounding changed to aggregate rounding")
	}
	second, ok := auditComponentProportion(amounts.pieces["BASE"], big.NewInt(2), big.NewInt(100), true)
	if !ok || second.Cmp(big.NewInt(3)) != 0 {
		t.Fatal("cumulative second receipt must retain each component ceiling")
	}
	third, ok := auditComponentProportion(amounts.pieces["BASE"], big.NewInt(3), big.NewInt(100), true)
	if !ok || third.Cmp(second) != 0 {
		t.Fatal("third receipt must allow zero rights increment")
	}
}

func TestReconciliationCorrectionRejectsUnprovenLineage(t *testing.T) {
	cases := []struct {
		section, key string
		row          int
		value        json.RawMessage
	}{
		{"corrections", "amount_microcredits", 0, json.RawMessage(`21`)},
		{"corrections", "wallet_id", 0, json.RawMessage(`"00000000-0000-0000-0000-000000000001"`)},
		{"corrections", "source_evidence_digest", 0, json.RawMessage(`"invalid"`)},
		{"corrections", "approval_epoch", 0, json.RawMessage(`0`)},
		{"correction_journals", "business_reference", 0, json.RawMessage(`"other"`)},
		{"correction_journals", "entry_type", 0, json.RawMessage(`"RECHARGE"`)},
		{"correction_postings", "delta_microcredits", 0, json.RawMessage(`-19`)},
		{"correction_postings", "credit_lot_id", 0, json.RawMessage(`"other"`)},
		{"correction_postings", "ledger_account_id", 0, json.RawMessage(`"other"`)},
		{"components", "reclassified_out_microcredits", 1, json.RawMessage(`19`)},
		{"components", "reclassified_out_microcredits", 0, json.RawMessage(`1`)},
		{"components", "original_microcredits", 2, json.RawMessage(`21`)},
		{"components", "component_type", 2, json.RawMessage(`"BONUS"`)},
		{"components", "credited_at", 2, json.RawMessage(`"2026-10-02T00:00:00Z"`)},
	}
	for _, c := range cases {
		t.Run(c.section+"/"+c.key, func(t *testing.T) {
			request, payload := correctionAuditFixture(t)
			before := reconciliationIntegrityDigest(payload)
			rows := auditRecords(payload[c.section])
			rows[c.row][c.key] = c.value
			payload[c.section], _ = json.Marshal(rows)
			fact := ReconciliationInternalFact{EntryType: "PAYMENT"}
			auditReconciliationFact(&fact, request, payload)
			if fact.LedgerValid || fact.IntegrityCaseType != "PAYMENT_PACKAGE_INVALID" {
				t.Fatal("tampered correction accepted")
			}
			if c.key != "credited_at" && reconciliationIntegrityDigest(payload) == before {
				t.Fatal("financial tampering reused retained identity")
			}
		})
	}
	for _, section := range []string{"corrections", "correction_journals", "correction_postings"} {
		t.Run("missing/"+section, func(t *testing.T) {
			request, payload := correctionAuditFixture(t)
			delete(payload, section)
			fact := ReconciliationInternalFact{EntryType: "PAYMENT"}
			auditReconciliationFact(&fact, request, payload)
			if fact.LedgerValid {
				t.Fatal("missing immutable correction evidence accepted")
			}
		})
	}
}
