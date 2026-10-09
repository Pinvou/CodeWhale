package account

import (
	"encoding/json"
	"github.com/google/uuid"
)

// Refund EDIT intent is derived only from its strict frozen proof artifact.
// Original refund/evidence/anomaly identifiers are never selected from live facts.
type approvalRefundExceptionRepairSummary struct {
	PaymentEvidenceID     uuid.UUID `json:"payment_evidence_id"`
	AnomalyCaseID         uuid.UUID `json:"anomaly_case_id"`
	RefundID              uuid.UUID `json:"refund_id"`
	WalletID              uuid.UUID `json:"wallet_id"`
	PackageID             uuid.UUID `json:"package_id"`
	AmountFen             string    `json:"amount_fen"`
	CreditMicrocredits    string    `json:"credit_microcredits"`
	RevokedMicrocredits   string    `json:"revoked_microcredits"`
	ShortfallMicrocredits string    `json:"shortfall_microcredits"`
	FactsDigest           string    `json:"facts_digest"`
	ReasonCode            string    `json:"reason_code"`
}

func approvalRefundFinancialDetail(target approvalRefundExceptionRepairSummary, encoded []byte) (approvalRefundExceptionRepairSummary, error) {
	var proof struct {
		Normalized struct {
			RefundID          uuid.UUID `json:"refund_id"`
			PaymentEvidenceID uuid.UUID `json:"payment_evidence_id"`
			AnomalyCaseID     uuid.UUID `json:"anomaly_case_id"`
			ReasonCode        string    `json:"reason_code"`
		} `json:"normalized_command"`
		Critical struct {
			FactsDigest string `json:"facts_digest"`
		} `json:"critical_resource_versions"`
		Financial struct {
			WalletID              uuid.UUID `json:"wallet_id"`
			PackageID             uuid.UUID `json:"package_id"`
			AmountFen             string    `json:"amount_fen"`
			CreditMicrocredits    string    `json:"credit_microcredits"`
			RevokedMicrocredits   string    `json:"revoked_microcredits"`
			ShortfallMicrocredits string    `json:"shortfall_microcredits"`
		} `json:"financial_snapshot"`
	}
	if json.Unmarshal(encoded, &proof) != nil || proof.Normalized.RefundID != target.RefundID || proof.Normalized.PaymentEvidenceID == uuid.Nil || proof.Normalized.AnomalyCaseID == uuid.Nil || !approvalPaginationHexDigest(proof.Critical.FactsDigest) || proof.Financial.WalletID == uuid.Nil || proof.Financial.PackageID == uuid.Nil || proof.Normalized.ReasonCode != "PROVIDER_REFUNDED_LOCAL_REVERSAL_MISSING" {
		return approvalRefundExceptionRepairSummary{}, ErrApprovalDetailPresentationUnavailable
	}
	target.PaymentEvidenceID = proof.Normalized.PaymentEvidenceID
	target.AnomalyCaseID = proof.Normalized.AnomalyCaseID
	target.WalletID = proof.Financial.WalletID
	target.PackageID = proof.Financial.PackageID
	target.AmountFen = proof.Financial.AmountFen
	target.CreditMicrocredits = proof.Financial.CreditMicrocredits
	target.RevokedMicrocredits = proof.Financial.RevokedMicrocredits
	target.ShortfallMicrocredits = proof.Financial.ShortfallMicrocredits
	target.FactsDigest = proof.Critical.FactsDigest
	target.ReasonCode = proof.Normalized.ReasonCode
	return target, nil
}
