package billing

import (
	"encoding/json"
	"math/big"
)

type refundRepairAuditAmounts struct {
	revoked, shortfall *big.Int
	lots               map[string]*big.Int
}

// The repair writer can reclaim each original reservation in full or leave it
// as a shortfall. Its immutable receipt distinguishes that split from an
// ordinary refund; a matching reference alone is never sufficient evidence.
func auditRefundRepairAmounts(request reconciliationAuditRequest, payload, source, transaction reconciliationAuditRecord,
	wallet, packageID string, credit *big.Int, originals, kinds map[string]*big.Int) (refundRepairAuditAmounts, bool) {
	invalid := refundRepairAuditAmounts{}
	journals, repairs := auditRecords(payload["journals"]), auditRecords(payload["refund_repairs"])
	if len(journals) != 1 || len(repairs) != 1 || auditString(source, "id") != request.Source || request.Source == "" {
		return invalid, false
	}
	journal, repair := journals[0], repairs[0]
	reference := "refund-repair:" + request.Source
	if auditString(journal, "business_reference") != reference || auditString(repair, "id") == "" ||
		auditString(repair, "refund_id") != request.Source || auditString(repair, "wallet_id") != wallet ||
		auditString(repair, "package_id") != packageID || auditString(repair, "journal_entry_id") != auditString(journal, "id") ||
		auditString(repair, "payment_evidence_id") == "" || auditString(repair, "state") != "EXECUTED" ||
		!bytesEqualJSONInteger(source["amount_fen"], transaction["amount_fen"]) {
		return invalid, false
	}
	refundCredit := auditInteger(source, "credit_microcredits")
	revoked, shortfall := auditInteger(repair, "revoked_microcredits"), auditInteger(repair, "shortfall_microcredits")
	if credit == nil || refundCredit == nil || refundCredit.Cmp(credit) != 0 || revoked == nil || shortfall == nil ||
		revoked.Sign() < 0 || shortfall.Sign() < 0 || new(big.Int).Add(revoked, shortfall).Cmp(credit) != 0 {
		return invalid, false
	}
	for kind, field := range map[string]string{"BASE": "base_microcredits", "BONUS": "bonus_microcredits"} {
		expected := kinds[kind]
		if expected == nil {
			expected = new(big.Int)
		}
		actual := auditInteger(source, field)
		if actual == nil || actual.Cmp(expected) != 0 {
			return invalid, false
		}
	}
	reserved := map[string]*big.Int{}
	components := map[string]reconciliationAuditRecord{}
	for _, component := range auditRecords(payload["components"]) {
		components[auditString(component, "credit_lot_id")] = component
	}
	reservationTotal := new(big.Int)
	for _, reservation := range auditRecords(payload["reservations"]) {
		lot := auditString(reservation, "credit_lot_id")
		amount := new(big.Int)
		for _, field := range []string{"available_microcredits", "active_grant_microcredits", "suspended_grant_microcredits"} {
			n := auditInteger(reservation, field)
			if n == nil || n.Sign() < 0 {
				return invalid, false
			}
			amount.Add(amount, n)
		}
		if lot == "" || reserved[lot] != nil || originals[lot] == nil || amount.Cmp(originals[lot]) != 0 ||
			auditString(reservation, "refund_id") != request.Source || auditString(reservation, "package_id") != packageID ||
			auditString(reservation, "component_id") != auditString(components[lot], "id") ||
			auditString(reservation, "component_type") != auditString(components[lot], "component_type") {
			return invalid, false
		}
		reserved[lot] = amount
		reservationTotal.Add(reservationTotal, amount)
	}
	if len(reserved) != len(originals) || reservationTotal.Cmp(credit) != 0 {
		return invalid, false
	}
	lots := map[string]*big.Int{}
	observedRevoked := new(big.Int)
	for lot := range reserved {
		lots[lot] = new(big.Int)
	}
	for _, post := range auditRecords(payload["postings"]) {
		if auditString(post, "journal_entry_id") != auditString(journal, "id") {
			return invalid, false
		}
		var account reconciliationAuditRecord
		_ = json.Unmarshal(post["account"], &account)
		if auditString(account, "account_type") != "WALLET_LIABILITY" {
			continue
		}
		n := auditInteger(post, "delta_microcredits")
		lot := auditString(post, "credit_lot_id")
		if n == nil || n.Sign() >= 0 || lots[lot] == nil {
			return invalid, false
		}
		lots[lot].Sub(lots[lot], n)
		observedRevoked.Sub(observedRevoked, n)
	}
	for lot, amount := range lots {
		if amount.Sign() != 0 && amount.Cmp(reserved[lot]) != 0 {
			return invalid, false
		}
	}
	if observedRevoked.Cmp(revoked) != 0 {
		return invalid, false
	}
	receivables := auditRecords(payload["receivables"])
	if shortfall.Sign() == 0 {
		if len(receivables) != 0 || auditString(repair, "receivable_id") != "" {
			return invalid, false
		}
	} else {
		if len(receivables) != 1 {
			return invalid, false
		}
		r := receivables[0]
		n := auditInteger(r, "original_microcredits")
		if n == nil || n.Cmp(shortfall) != 0 || auditString(r, "id") != auditString(repair, "receivable_id") ||
			auditString(r, "source_type") != "FORCED_REFUND" || auditString(r, "source_reference") != reference ||
			auditString(r, "wallet_id") != wallet || auditString(r, "payment_order_id") != request.Order ||
			auditString(r, "payment_transaction_id") != request.Transaction || auditString(r, "entitlement_package_id") != packageID {
			return invalid, false
		}
	}
	return refundRepairAuditAmounts{revoked: revoked, shortfall: shortfall, lots: lots}, true
}
