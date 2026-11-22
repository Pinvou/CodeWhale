package billing

import (
	"encoding/json"
	"testing"
)

func refundRepairAuditFixture(t *testing.T, shortfall bool) (ReconciliationInternalFact, reconciliationAuditRequest, reconciliationAuditRecord) {
	t.Helper()
	ref := "refund-repair:refund"
	post := func(account, kind, code, lot string, delta int64) map[string]any {
		wallet := ""
		if kind == "WALLET_LIABILITY" {
			wallet = "wallet"
		}
		return map[string]any{"journal_entry_id": "journal", "credit_lot_id": lot, "delta_microcredits": delta,
			"account": map[string]any{"id": account, "wallet_id": wallet, "account_type": kind, "code": code}}
	}
	revoked, debt := int64(100), int64(0)
	posts := []any{post("wallet-account", "WALLET_LIABILITY", "wallet:wallet", "base-lot", -60)}
	receivables := []any{}
	receivableID := ""
	if shortfall {
		revoked, debt, receivableID = 60, 40, "debt"
		posts = append(posts, post(receivableAccount.String(), "RECEIVABLE_ASSET", "platform:receivables", "", -40))
		receivables = append(receivables, map[string]any{"id": "debt", "wallet_id": "wallet", "source_type": "FORCED_REFUND", "source_reference": ref,
			"original_microcredits": 40, "payment_order_id": "order", "payment_transaction_id": "transaction", "entitlement_package_id": "package"})
	} else {
		posts = append(posts, post("wallet-account", "WALLET_LIABILITY", "wallet:wallet", "bonus-lot", -40))
	}
	posts = append(posts, post(refundClearingAccount.String(), "REFUND_CLEARING", "platform:refund-clearing", "", 100))
	source := map[string]any{"id": "refund", "payment_order_id": "order", "payment_transaction_id": "transaction", "entitlement_package_id": "package",
		"amount_fen": 100, "credit_microcredits": 100, "base_microcredits": 60, "bonus_microcredits": 40}
	components, reservations := []any{}, []any{}
	for _, item := range []struct {
		kind, lot, suffix string
		amount            int64
	}{{"BASE", "base-lot", "", 60}, {"BONUS", "bonus-lot", ":bonus", 40}} {
		components = append(components, map[string]any{"id": item.lot, "package_id": "package", "component_type": item.kind, "credit_lot_id": item.lot, "original_microcredits": item.amount,
			"lot": map[string]any{"id": item.lot, "wallet_id": "wallet", "source_type": "RECHARGE", "source_reference": "payment-transaction:transaction" + item.suffix, "original_microcredits": item.amount}})
		reservations = append(reservations, map[string]any{"refund_id": "refund", "component_id": item.lot, "package_id": "package", "component_type": item.kind,
			"credit_lot_id": item.lot, "available_microcredits": item.amount, "active_grant_microcredits": 0, "suspended_grant_microcredits": 0})
	}
	v := map[string]any{
		"order": map[string]any{"id": "order", "wallet_id": "wallet", "requester_id": "owner", "amount_fen": 100, "currency": "CNY", "credit_microcredits": 100,
			"product_snapshot": Product{AmountFen: 100, CreditMicrocredits: 100, BasePoints: 60, BonusPoints: 40, Currency: "CNY"}},
		"transaction": map[string]any{"id": "transaction", "payment_order_id": "order", "amount_fen": 100, "currency": "CNY"},
		"packages":    []any{map[string]any{"id": "package", "user_id": "owner", "wallet_id": "wallet", "source_type": "PAYMENT", "payment_order_id": "order", "source_payment_transaction_id": "transaction", "points_total": 100, "amount_fen": 100}},
		"components":  components, "reservations": reservations, "postings": posts, "receivables": receivables,
		"journals": []any{map[string]any{"id": "journal", "entry_type": "REFUND", "business_reference": ref}},
		"refund_repairs": []any{map[string]any{"id": "repair", "refund_id": "refund", "wallet_id": "wallet", "package_id": "package", "state": "EXECUTED",
			"revoked_microcredits": revoked, "shortfall_microcredits": debt, "payment_evidence_id": "evidence", "journal_entry_id": "journal", "receivable_id": receivableID}},
	}
	raw, _ := json.Marshal(v)
	var payload reconciliationAuditRecord
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	sourceRaw, _ := json.Marshal(source)
	return ReconciliationInternalFact{EntryType: "REFUND", RefundSnapshot: sourceRaw}, reconciliationAuditRequest{Order: "order", Transaction: "transaction", Source: "refund", References: []string{"refund:refund", ref}}, payload
}

func TestRefundRepairReconciliationAuditKeepsExactFacts(t *testing.T) {
	for _, shortfall := range []bool{false, true} {
		name := "full-reclaim"
		if shortfall {
			name = "with-shortfall"
		}
		t.Run(name, func(t *testing.T) {
			fact, request, payload := refundRepairAuditFixture(t, shortfall)
			auditReconciliationFact(&fact, request, payload)
			if !fact.LedgerPresent || !fact.LedgerValid || fact.IntegrityCaseType != "" {
				t.Fatalf("valid repair rejected: %+v", fact)
			}
		})
	}
	for _, scenario := range []struct {
		name    string
		section string
		field   string
		value   json.RawMessage
	}{
		{"wrong-refund", "refund_repairs", "refund_id", json.RawMessage(`"foreign"`)},
		{"wrong-wallet", "refund_repairs", "wallet_id", json.RawMessage(`"foreign"`)},
		{"wrong-package", "refund_repairs", "package_id", json.RawMessage(`"foreign"`)},
		{"wrong-journal", "refund_repairs", "journal_entry_id", json.RawMessage(`"foreign"`)},
		{"missing-evidence", "refund_repairs", "payment_evidence_id", json.RawMessage(`""`)},
		{"wrong-state", "refund_repairs", "state", json.RawMessage(`"PENDING"`)},
		{"negative-revoked", "refund_repairs", "revoked_microcredits", json.RawMessage(`-1`)},
		{"excess-debt", "refund_repairs", "shortfall_microcredits", json.RawMessage(`41`)},
		{"wrong-debt-reference", "receivables", "source_reference", json.RawMessage(`"refund:refund"`)},
		{"wrong-debt-wallet", "receivables", "wallet_id", json.RawMessage(`"foreign"`)},
		{"wrong-debt-transaction", "receivables", "payment_transaction_id", json.RawMessage(`"foreign"`)},
		{"wrong-debt-type", "receivables", "source_type", json.RawMessage(`"CHARGEBACK"`)},
		{"wrong-debt-amount", "receivables", "original_microcredits", json.RawMessage(`39`)},
		{"wrong-debt-identity", "refund_repairs", "receivable_id", json.RawMessage(`"foreign"`)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fact, request, payload := refundRepairAuditFixture(t, true)
			rows := auditRecords(payload[scenario.section])
			rows[0][scenario.field] = scenario.value
			payload[scenario.section], _ = json.Marshal(rows)
			auditReconciliationFact(&fact, request, payload)
			if fact.LedgerValid || fact.IntegrityCaseType != "REFUND_LEDGER_INVALID" {
				t.Fatal("mismatched repair facts accepted")
			}
		})
	}
	t.Run("foreign-reference-full-reclaim", func(t *testing.T) {
		fact, request, payload := refundRepairAuditFixture(t, false)
		rows := auditRecords(payload["journals"])
		rows[0]["business_reference"] = json.RawMessage(`"refund-repair:foreign"`)
		payload["journals"], _ = json.Marshal(rows)
		auditReconciliationFact(&fact, request, payload)
		if fact.LedgerValid || fact.IntegrityCaseType != "REFUND_LEDGER_INVALID" {
			t.Fatal("another refund's repair reference accepted")
		}
	})
	for _, section := range []string{"refund_repairs", "journals"} {
		for _, duplicate := range []bool{false, true} {
			name := "missing-" + section
			if duplicate {
				name = "duplicate-" + section
			}
			t.Run(name, func(t *testing.T) {
				fact, request, payload := refundRepairAuditFixture(t, true)
				rows := auditRecords(payload[section])
				if duplicate {
					rows = append(rows, rows[0])
				} else {
					rows = nil
				}
				payload[section], _ = json.Marshal(rows)
				auditReconciliationFact(&fact, request, payload)
				if fact.LedgerValid {
					t.Fatal("missing or ambiguous evidence accepted")
				}
			})
		}
	}
}

func TestRefundRepairReconciliationDigestKeepsStableFacts(t *testing.T) {
	_, _, payload := refundRepairAuditFixture(t, true)
	delete(payload, "refund_repairs")
	before := reconciliationIntegrityDigest(payload)
	payload["refund_repairs"] = json.RawMessage(`[]`)
	if reconciliationIntegrityDigest(payload) != before {
		t.Fatal("empty new receipt collection changed an old incident identity")
	}
	_, _, payload = refundRepairAuditFixture(t, true)
	before = reconciliationIntegrityDigest(payload)
	rows := auditRecords(payload["refund_repairs"])
	rows[0]["created_at"] = json.RawMessage(`"changed-clock"`)
	payload["refund_repairs"], _ = json.Marshal(rows)
	if reconciliationIntegrityDigest(payload) != before {
		t.Fatal("receipt clock changed retained identity")
	}
	for _, field := range []string{"refund_id", "wallet_id", "package_id", "journal_entry_id", "receivable_id", "revoked_microcredits", "shortfall_microcredits"} {
		t.Run(field, func(t *testing.T) {
			_, _, p := refundRepairAuditFixture(t, true)
			rows := auditRecords(p["refund_repairs"])
			rows[0][field] = json.RawMessage(`"changed-fact"`)
			p["refund_repairs"], _ = json.Marshal(rows)
			if reconciliationIntegrityDigest(p) == before {
				t.Fatal("changed immutable receipt retained incident identity")
			}
		})
	}
}
