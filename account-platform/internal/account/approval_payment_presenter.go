package account

import (
	"encoding/json"
	"github.com/google/uuid"
)

// Payment detail is populated only from the strict normalized proof artifact.
// This retains original order/evidence intent for EDIT; no live order lookup or
// missing evidence fallback may alter the command selected by its requester.
type approvalPaymentFinancialRepairSummary struct {
	PaymentEvidenceID    uuid.UUID `json:"payment_evidence_id"`
	PaymentOrderID       uuid.UUID `json:"payment_order_id"`
	AmountFen            string    `json:"amount_fen"`
	CreditMicrocredits   string    `json:"credit_microcredits"`
	AppliedToReceivables string    `json:"applied_to_receivables"`
	WalletVersion        string    `json:"wallet_version"`
	FactsDigest          string    `json:"facts_digest"`
	ReasonCode           string    `json:"reason_code"`
}

func approvalPaymentFinancialDetail(target approvalPaymentFinancialRepairSummary, encoded []byte) (approvalPaymentFinancialRepairSummary, error) {
	var proof struct {
		Normalized struct {
			PaymentOrderID    uuid.UUID `json:"payment_order_id"`
			PaymentEvidenceID uuid.UUID `json:"payment_evidence_id"`
			ReasonCode        string    `json:"reason_code"`
		} `json:"normalized_command"`
		Critical struct {
			FactsDigest           string `json:"facts_digest"`
			PaymentOrderUpdatedAt string `json:"payment_order_updated_at"`
		} `json:"critical_resource_versions"`
		Financial struct {
			AmountFen            string `json:"amount_fen"`
			CreditMicrocredits   string `json:"credit_microcredits"`
			AppliedToReceivables string `json:"applied_to_receivables"`
			WalletVersion        string `json:"wallet_version"`
		} `json:"financial_snapshot"`
	}
	if json.Unmarshal(encoded, &proof) != nil || proof.Normalized.PaymentEvidenceID == uuid.Nil || proof.Normalized.PaymentOrderID != target.PaymentOrderID || !approvalPaginationHexDigest(proof.Critical.FactsDigest) || proof.Critical.PaymentOrderUpdatedAt == "" || proof.Normalized.ReasonCode != "CONFIRMED_PAYMENT_NOT_CREDITED" {
		return approvalPaymentFinancialRepairSummary{}, ErrApprovalDetailPresentationUnavailable
	}
	target.PaymentEvidenceID = proof.Normalized.PaymentEvidenceID
	target.AmountFen = proof.Financial.AmountFen
	target.CreditMicrocredits = proof.Financial.CreditMicrocredits
	target.AppliedToReceivables = proof.Financial.AppliedToReceivables
	target.WalletVersion = proof.Financial.WalletVersion
	target.FactsDigest = proof.Critical.FactsDigest
	target.ReasonCode = proof.Normalized.ReasonCode
	return target, nil
}
