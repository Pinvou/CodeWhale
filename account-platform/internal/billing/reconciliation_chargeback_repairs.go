package billing

// Validate the selected receipt's immutable origin, without reapplying today's
// wallet balances or execution eligibility to a historical compensation.
func auditChargebackJournalReference(request reconciliationAuditRequest, payload, source, journal reconciliationAuditRecord, wallet, packageID string) bool {
	legacy := "payment-transaction:" + request.Transaction + ":chargeback:" + auditString(source, "provider_chargeback_id")
	reference := auditString(journal, "business_reference")
	if reference == legacy {
		return true
	}
	if reference != chargebackRepairReferencePrefix+request.Source || request.Source == "" || auditString(source, "id") != request.Source {
		return false
	}
	cases, debts := auditRecords(payload["chargeback_repairs"]), auditRecords(payload["receivables"])
	if len(cases) != 1 || len(debts) != 1 {
		return false
	}
	c, debt := cases[0], debts[0]
	if auditString(c, "id") == "" || auditString(c, "state") != "EXECUTED" ||
		auditString(c, "chargeback_case_id") != request.Source || auditString(c, "wallet_id") != wallet || auditString(c, "package_id") != packageID ||
		auditString(c, "anomaly_case_id") == "" || auditString(c, "anomaly_case_id") != auditString(source, "manual_review_case_id") ||
		auditString(c, "payment_evidence_id") == "" || auditString(c, "payment_evidence_id") != auditString(source, "payment_evidence_id") ||
		auditString(c, "journal_entry_id") != auditString(journal, "id") || auditString(journal, "id") == "" ||
		auditString(c, "receivable_id") == "" || auditString(c, "receivable_id") != auditString(source, "receivable_id") || auditString(c, "receivable_id") != auditString(debt, "id") ||
		auditString(source, "status") != "CONFIRMED" || auditString(source, "resolution_code") != "CONFIRM_CHARGEBACK_SHORTFALL" {
		return false
	}
	amount := auditInteger(c, "base_shortfall_microcredits")
	original := auditInteger(debt, "original_microcredits")
	base, bonus := auditInteger(source, "revoked_base_microcredits"), auditInteger(source, "revoked_bonus_microcredits")
	if amount == nil || original == nil || amount.Sign() <= 0 || amount.Cmp(original) != 0 || base == nil || bonus == nil || base.Sign() != 0 || bonus.Sign() != 0 {
		return false
	}
	// The repair writer has exactly two un-lotted legs. The surrounding audit
	// still verifies the BASE amount, debt provenance, accounts, signs and net.
	posts := auditRecords(payload["postings"])
	if len(posts) != 2 {
		return false
	}
	for _, post := range posts {
		if auditString(post, "journal_entry_id") != auditString(journal, "id") || auditString(post, "credit_lot_id") != "" {
			return false
		}
	}
	return true
}
