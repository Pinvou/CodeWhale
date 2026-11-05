package billing

import (
	"encoding/json"
	"testing"
)

func chargebackRepairAuditFixture(t *testing.T) (ReconciliationInternalFact, reconciliationAuditRequest, reconciliationAuditRecord) {
	t.Helper()
	ref := "chargeback-repair:chargeback"
	post := func(account, kind, code, lot string, delta int64) map[string]any {
		wallet := ""
		if kind == "WALLET_LIABILITY" {
			wallet = "wallet"
		}
		return map[string]any{"journal_entry_id": "journal", "credit_lot_id": lot, "delta_microcredits": delta,
			"account": map[string]any{"id": account, "wallet_id": wallet, "account_type": kind, "code": code}}
	}
	posts := []any{post(receivableAccount.String(), "RECEIVABLE_ASSET", "platform:receivables", "", -60), post(chargebackClearingAccount.String(), "CHARGEBACK_CLEARING", "platform:chargeback-clearing", "", 60)}
	receivables := []any{map[string]any{"id": "debt", "wallet_id": "wallet", "source_type": "CHARGEBACK", "source_reference": ref, "original_microcredits": 60, "payment_order_id": "order", "payment_transaction_id": "transaction", "entitlement_package_id": "package"}}
	source := map[string]any{"id": "chargeback", "manual_review_case_id": "anomaly", "payment_evidence_id": "evidence", "wallet_id": "wallet", "status": "CONFIRMED", "resolution_code": "CONFIRM_CHARGEBACK_SHORTFALL", "receivable_id": "debt", "provider_chargeback_id": "provider", "revoked_base_microcredits": 0, "revoked_bonus_microcredits": 0, "payment_order_id": "order", "payment_transaction_id": "transaction", "entitlement_package_id": "package",
		"amount_fen": 100, "credit_microcredits": 100, "base_microcredits": 60, "bonus_microcredits": 40}
	components := []any{}
	for _, item := range []struct {
		kind, lot, suffix string
		amount            int64
	}{{"BASE", "base-lot", "", 60}, {"BONUS", "bonus-lot", ":bonus", 40}} {
		components = append(components, map[string]any{"id": item.lot, "package_id": "package", "component_type": item.kind, "credit_lot_id": item.lot, "original_microcredits": item.amount,
			"lot": map[string]any{"id": item.lot, "wallet_id": "wallet", "source_type": "RECHARGE", "source_reference": "payment-transaction:transaction" + item.suffix, "original_microcredits": item.amount}})

	}
	v := map[string]any{
		"order": map[string]any{"id": "order", "wallet_id": "wallet", "requester_id": "owner", "amount_fen": 100, "currency": "CNY", "credit_microcredits": 100,
			"product_snapshot": Product{AmountFen: 100, CreditMicrocredits: 100, BasePoints: 60, BonusPoints: 40, Currency: "CNY"}},
		"transaction": map[string]any{"id": "transaction", "payment_order_id": "order", "amount_fen": 100, "currency": "CNY"},
		"packages":    []any{map[string]any{"id": "package", "user_id": "owner", "wallet_id": "wallet", "source_type": "PAYMENT", "payment_order_id": "order", "source_payment_transaction_id": "transaction", "points_total": 100, "amount_fen": 100}},
		"components":  components, "postings": posts, "receivables": receivables,
		"journals":           []any{map[string]any{"id": "journal", "entry_type": "CHARGEBACK", "business_reference": ref}},
		"chargeback_repairs": []any{map[string]any{"id": "repair", "anomaly_case_id": "anomaly", "chargeback_case_id": "chargeback", "wallet_id": "wallet", "package_id": "package", "state": "EXECUTED", "base_shortfall_microcredits": 60, "payment_evidence_id": "evidence", "journal_entry_id": "journal", "receivable_id": "debt"}},
	}
	raw, _ := json.Marshal(v)
	var payload reconciliationAuditRecord
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	sourceRaw, _ := json.Marshal(source)
	return ReconciliationInternalFact{EntryType: "CHARGEBACK", RefundSnapshot: sourceRaw}, reconciliationAuditRequest{Order: "order", Transaction: "transaction", Source: "chargeback", References: []string{"payment-transaction:transaction:chargeback:provider", ref}}, payload
}

func TestChargebackRepairReconciliationAuditKeepsExactFacts(t *testing.T) {
	fact, request, payload := chargebackRepairAuditFixture(t)
	auditReconciliationFact(&fact, request, payload)
	if !fact.LedgerPresent || !fact.LedgerValid || fact.IntegrityCaseType != "" {
		t.Fatalf("valid BASE-only compensation rejected: %+v", fact)
	}
	for _, scenario := range []struct{ section, field, value string }{
		{"chargeback_repairs", "chargeback_case_id", `"foreign"`},
		{"chargeback_repairs", "anomaly_case_id", `"foreign"`},
		{"chargeback_repairs", "wallet_id", `"foreign"`},
		{"chargeback_repairs", "package_id", `"foreign"`},
		{"chargeback_repairs", "payment_evidence_id", `"foreign"`},
		{"chargeback_repairs", "journal_entry_id", `"foreign"`},
		{"chargeback_repairs", "receivable_id", `"foreign"`},
		{"chargeback_repairs", "state", `"PENDING"`},
		{"chargeback_repairs", "base_shortfall_microcredits", `-1`},
		{"chargeback_repairs", "base_shortfall_microcredits", `61`},
		{"receivables", "source_reference", `"chargeback-repair:foreign"`},
		{"receivables", "wallet_id", `"foreign"`},
		{"receivables", "payment_order_id", `"foreign"`},
		{"receivables", "payment_transaction_id", `"foreign"`},
		{"receivables", "entitlement_package_id", `"foreign"`},
		{"receivables", "source_type", `"FORCED_REFUND"`},
		{"receivables", "original_microcredits", `59`},
		{"journals", "business_reference", `"chargeback-repair:foreign"`},
		{"postings", "delta_microcredits", `60`},
		{"postings", "credit_lot_id", `"base-lot"`},
		{"postings", "journal_entry_id", `"foreign"`},
	} {
		t.Run(scenario.section+"/"+scenario.field, func(t *testing.T) {
			fact, request, payload := chargebackRepairAuditFixture(t)
			rows := auditRecords(payload[scenario.section])
			rows[0][scenario.field] = json.RawMessage(scenario.value)
			payload[scenario.section], _ = json.Marshal(rows)
			auditReconciliationFact(&fact, request, payload)
			if fact.LedgerValid || fact.IntegrityCaseType != "CHARGEBACK_LEDGER_INVALID" {
				t.Fatal("foreign or malformed compensation accepted")
			}
		})
	}
	for key, value := range map[string]string{"status": `"REVIEW_REQUIRED"`, "resolution_code": `"CONFIRM_CHARGEBACK"`, "revoked_base_microcredits": `1`, "revoked_bonus_microcredits": `1`, "wallet_id": `"foreign"`, "payment_order_id": `"foreign"`, "payment_transaction_id": `"foreign"`, "entitlement_package_id": `"foreign"`} {
		t.Run("source/"+key, func(t *testing.T) {
			fact, request, payload := chargebackRepairAuditFixture(t)
			var source reconciliationAuditRecord
			_ = json.Unmarshal(fact.RefundSnapshot, &source)
			source[key] = json.RawMessage(value)
			fact.RefundSnapshot, _ = json.Marshal(source)
			auditReconciliationFact(&fact, request, payload)
			if fact.LedgerValid || fact.IntegrityCaseType != "CHARGEBACK_LEDGER_INVALID" {
				t.Fatal("wrong source admitted")
			}
		})
	}
	for _, section := range []string{"chargeback_repairs", "journals"} {
		for _, duplicate := range []bool{false, true} {
			name := "missing-" + section
			if duplicate {
				name = "duplicate-" + section
			}
			t.Run(name, func(t *testing.T) {
				fact, request, payload := chargebackRepairAuditFixture(t)
				rows := auditRecords(payload[section])
				if duplicate {
					rows = append(rows, rows[0])
					if section == "journals" {
						other := reconciliationAuditRecord{}
						for k, v := range rows[0] {
							other[k] = v
						}
						other["business_reference"] = json.RawMessage(`"payment-transaction:transaction:chargeback:provider"`)
						rows[1] = other
					}
				} else {
					rows = nil
				}
				payload[section], _ = json.Marshal(rows)
				auditReconciliationFact(&fact, request, payload)
				if fact.LedgerValid {
					t.Fatal("ambiguous or missing receipt accepted")
				}
			})
		}
	}
}

func TestChargebackRepairIntegrityIdentityUsesStableReceipt(t *testing.T) {
	_, _, payload := chargebackRepairAuditFixture(t)
	before := reconciliationIntegrityDigest(payload)
	rows := auditRecords(payload["chargeback_repairs"])
	rows[0]["created_at"] = json.RawMessage(`"later"`)
	payload["chargeback_repairs"], _ = json.Marshal(rows)
	if reconciliationIntegrityDigest(payload) != before {
		t.Fatal("clock changed identity")
	}
	rows[0]["base_shortfall_microcredits"] = json.RawMessage(`61`)
	payload["chargeback_repairs"], _ = json.Marshal(rows)
	if reconciliationIntegrityDigest(payload) == before {
		t.Fatal("changed compensation reused identity")
	}
	delete(payload, "chargeback_repairs")
	before = reconciliationIntegrityDigest(payload)
	payload["chargeback_repairs"] = json.RawMessage(`[]`)
	if reconciliationIntegrityDigest(payload) != before {
		t.Fatal("empty new collection changed legacy identity")
	}
}
