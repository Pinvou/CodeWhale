package account

import (
	"encoding/json"
	"github.com/google/uuid"
)

// Usage EDIT preserves the original hold, request and loss-review identifiers.
// The strict encoded proof is the source of exact frozen facts, never live owner replacements.
type approvalUsageSettlementRepairSummary struct {
	LossReviewID         uuid.UUID `json:"loss_review_id"`
	HoldID               uuid.UUID `json:"hold_id"`
	UsageRequestID       uuid.UUID `json:"usage_request_id"`
	WalletID             uuid.UUID `json:"wallet_id"`
	EvidenceKind         string    `json:"evidence_kind"`
	ExpectedMicrocredits string    `json:"expected_microcredits"`
	SettledMicrocredits  string    `json:"settled_microcredits"`
	OverageMicrocredits  string    `json:"overage_microcredits"`
	AllocationsDigest    string    `json:"allocations_digest"`
	FactsDigest          string    `json:"facts_digest"`
	ReasonCode           string    `json:"reason_code"`
}

func approvalUsageSettlementDetail(target approvalUsageSettlementRepairSummary, encoded []byte) (approvalUsageSettlementRepairSummary, error) {
	var proof struct {
		Normalized struct {
			UsageRequestID uuid.UUID `json:"usage_request_id"`
			HoldID         uuid.UUID `json:"hold_id"`
			LossReviewID   uuid.UUID `json:"loss_review_id"`
			EvidenceKind   string    `json:"evidence_kind"`
			ReasonCode     string    `json:"reason_code"`
		} `json:"normalized_command"`
		Critical struct {
			HoldUpdatedAt       string `json:"hold_updated_at"`
			LossReviewUpdatedAt string `json:"loss_review_updated_at"`
			FactsDigest         string `json:"facts_digest"`
		} `json:"critical_resource_versions"`
		Financial struct {
			WalletID             uuid.UUID `json:"wallet_id"`
			ExpectedMicrocredits string    `json:"expected_microcredits"`
			SettledMicrocredits  string    `json:"settled_microcredits"`
			OverageMicrocredits  string    `json:"overage_microcredits"`
			AllocationsDigest    string    `json:"allocations_digest"`
		} `json:"financial_snapshot"`
	}
	if json.Unmarshal(encoded, &proof) != nil || proof.Normalized.HoldID != target.HoldID || proof.Normalized.UsageRequestID == uuid.Nil || proof.Normalized.LossReviewID == uuid.Nil || proof.Normalized.EvidenceKind != "LOSS_REVIEW" || proof.Normalized.ReasonCode != "MISSING_USAGE_SETTLEMENT" || proof.Critical.HoldUpdatedAt == "" || proof.Critical.LossReviewUpdatedAt == "" || !approvalPaginationHexDigest(proof.Critical.FactsDigest) || !approvalPaginationHexDigest(proof.Financial.AllocationsDigest) || proof.Financial.WalletID == uuid.Nil {
		return approvalUsageSettlementRepairSummary{}, ErrApprovalDetailPresentationUnavailable
	}
	target.UsageRequestID = proof.Normalized.UsageRequestID
	target.LossReviewID = proof.Normalized.LossReviewID
	target.WalletID = proof.Financial.WalletID
	target.EvidenceKind = proof.Normalized.EvidenceKind
	target.ReasonCode = proof.Normalized.ReasonCode
	target.FactsDigest = proof.Critical.FactsDigest
	target.AllocationsDigest = proof.Financial.AllocationsDigest
	target.ExpectedMicrocredits = proof.Financial.ExpectedMicrocredits
	target.SettledMicrocredits = proof.Financial.SettledMicrocredits
	target.OverageMicrocredits = proof.Financial.OverageMicrocredits
	return target, nil
}
