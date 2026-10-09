package account

import (
	"encoding/json"
	"github.com/google/uuid"
)

// Chargeback EDIT uses only the strict frozen proof: its original case/evidence
// and expected case version must not be replaced by current owner facts.
type approvalChargebackFinancialRepairSummary struct {
	PaymentEvidenceID         uuid.UUID `json:"payment_evidence_id"`
	ExpectedCaseVersion       string    `json:"expected_case_version"`
	ChargebackCaseID          uuid.UUID `json:"chargeback_case_id"`
	WalletID                  uuid.UUID `json:"wallet_id"`
	PackageID                 uuid.UUID `json:"package_id"`
	BaseShortfallMicrocredits string    `json:"base_shortfall_microcredits"`
	ComponentDigest           string    `json:"component_digest"`
	FactsDigest               string    `json:"facts_digest"`
	ReasonCode                string    `json:"reason_code"`
}

func approvalChargebackFinancialDetail(target approvalChargebackFinancialRepairSummary, encoded []byte) (approvalChargebackFinancialRepairSummary, error) {
	var proof struct {
		Normalized struct {
			ChargebackCaseID    uuid.UUID `json:"chargeback_case_id"`
			PaymentEvidenceID   uuid.UUID `json:"payment_evidence_id"`
			ExpectedCaseVersion string    `json:"expected_case_version"`
			ReasonCode          string    `json:"reason_code"`
		} `json:"normalized_command"`
		Critical struct {
			ChargebackCaseVersion string `json:"chargeback_case_version"`
			FactsDigest           string `json:"facts_digest"`
		} `json:"critical_resource_versions"`
		Financial struct {
			WalletID                  uuid.UUID `json:"wallet_id"`
			PackageID                 uuid.UUID `json:"package_id"`
			BaseShortfallMicrocredits string    `json:"base_shortfall_microcredits"`
			ComponentDigest           string    `json:"component_digest"`
		} `json:"financial_snapshot"`
	}
	if json.Unmarshal(encoded, &proof) != nil || proof.Normalized.ChargebackCaseID != target.ChargebackCaseID || proof.Normalized.PaymentEvidenceID == uuid.Nil || proof.Normalized.ExpectedCaseVersion == "" || proof.Normalized.ExpectedCaseVersion != proof.Critical.ChargebackCaseVersion || !approvalPaginationHexDigest(proof.Critical.FactsDigest) || !approvalPaginationHexDigest(proof.Financial.ComponentDigest) || proof.Financial.WalletID == uuid.Nil || proof.Financial.PackageID == uuid.Nil || proof.Normalized.ReasonCode != "BASE_SHORTFALL_COMPENSATION" {
		return approvalChargebackFinancialRepairSummary{}, ErrApprovalDetailPresentationUnavailable
	}
	target.PaymentEvidenceID = proof.Normalized.PaymentEvidenceID
	target.ExpectedCaseVersion = proof.Normalized.ExpectedCaseVersion
	target.WalletID = proof.Financial.WalletID
	target.PackageID = proof.Financial.PackageID
	target.BaseShortfallMicrocredits = proof.Financial.BaseShortfallMicrocredits
	target.ComponentDigest = proof.Financial.ComponentDigest
	target.FactsDigest = proof.Critical.FactsDigest
	target.ReasonCode = proof.Normalized.ReasonCode
	return target, nil
}
