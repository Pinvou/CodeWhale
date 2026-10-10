package account

import (
	"encoding/json"
	"github.com/google/uuid"
)

// EDIT preserves the validated frozen case, evidence and business date.
// Current owner facts never replace the original intent.
type approvalReconciliationFinancialRepairSummary struct {
	PaymentEvidenceID    uuid.UUID `json:"payment_evidence_id"`
	ReconciliationCaseID uuid.UUID `json:"reconciliation_case_id"`
	RunID                uuid.UUID `json:"run_id"`
	BusinessDate         string    `json:"business_date"`
	AmountFen            string    `json:"amount_fen"`
	Currency             string    `json:"currency"`
	LedgerEffect         string    `json:"ledger_effect"`
	FactsDigest          string    `json:"facts_digest"`
	ResolutionCode       string    `json:"resolution_code"`
}

func approvalReconciliationFinancialDetail(target approvalReconciliationFinancialRepairSummary, encoded []byte) (approvalReconciliationFinancialRepairSummary, error) {
	var proof struct {
		Normalized struct {
			ReconciliationCaseID uuid.UUID `json:"reconciliation_case_id"`
			PaymentEvidenceID    uuid.UUID `json:"payment_evidence_id"`
			BusinessDate         string    `json:"business_date"`
			ResolutionCode       string    `json:"resolution_code"`
		} `json:"normalized_command"`
		Critical struct {
			RunID                  uuid.UUID `json:"run_id"`
			CaseID                 uuid.UUID `json:"case_id"`
			FactsDigest            string    `json:"facts_digest"`
			ProviderSnapshotDigest string    `json:"provider_snapshot_digest"`
		} `json:"critical_resource_versions"`
		Financial struct {
			AmountFen    string `json:"amount_fen"`
			Currency     string `json:"currency"`
			LedgerEffect string `json:"ledger_effect"`
		} `json:"financial_snapshot"`
	}
	if json.Unmarshal(encoded, &proof) != nil || proof.Normalized.ReconciliationCaseID != target.ReconciliationCaseID || proof.Critical.CaseID != target.ReconciliationCaseID || proof.Normalized.PaymentEvidenceID == uuid.Nil || proof.Critical.RunID == uuid.Nil || !approvalPaginationHexDigest(proof.Critical.FactsDigest) || !approvalPaginationHexDigest(proof.Critical.ProviderSnapshotDigest) || proof.Normalized.ResolutionCode != "PROVIDER_REFUND_CONFIRMED" || proof.Financial.LedgerEffect != "NONE" {
		return approvalReconciliationFinancialRepairSummary{}, ErrApprovalDetailPresentationUnavailable
	}
	target.PaymentEvidenceID = proof.Normalized.PaymentEvidenceID
	target.RunID = proof.Critical.RunID
	target.BusinessDate = proof.Normalized.BusinessDate
	target.AmountFen = proof.Financial.AmountFen
	target.Currency = proof.Financial.Currency
	target.LedgerEffect = proof.Financial.LedgerEffect
	target.FactsDigest = proof.Critical.FactsDigest
	target.ResolutionCode = proof.Normalized.ResolutionCode
	return target, nil
}
