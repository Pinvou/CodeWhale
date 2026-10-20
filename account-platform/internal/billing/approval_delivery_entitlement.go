package billing

import (
	"context"
	"encoding/json"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
)

func entitlementAuthorizationResult(result EntitlementCorrectionResult) (json.RawMessage, json.RawMessage, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, nil, err
	}
	ledger, err := json.Marshal(map[string]any{"journal_entry_id": result.JournalEntryID})
	return encoded, ledger, err
}

func (domain *billingAuthorizationDomain) executeEntitlementCorrection(ctx context.Context, binding ApprovalAuthorizationBinding, event approvalencoding.AuthorizationEvent, command ApprovedCommand) authorizationDomainAttempt {
	var normalized struct {
		PackageID      uuid.UUID `json:"package_id"`
		PaymentOrderID uuid.UUID `json:"payment_order_id"`
		ReasonCode     string    `json:"reason_code"`
	}
	var critical struct {
		PackageID            uuid.UUID `json:"package_id"`
		PaymentOrderID       uuid.UUID `json:"payment_order_id"`
		PolicyVersion        int64     `json:"policy_version"`
		WalletID             uuid.UUID `json:"wallet_id"`
		WalletVersion        int64     `json:"wallet_version"`
		PaymentTransactionID uuid.UUID `json:"payment_transaction_id"`
		SourceComponentID    uuid.UUID `json:"source_component_id"`
		SourceLotID          uuid.UUID `json:"source_lot_id"`
		TargetComponentID    uuid.UUID `json:"target_component_id"`
		TargetLotID          uuid.UUID `json:"target_lot_id"`
		SourceType           string    `json:"source_type"`
		TargetType           string    `json:"target_type"`
		AmountMicrocredits   int64     `json:"amount_microcredits"`
		SourceEvidenceDigest string    `json:"source_evidence_digest"`
		FactsDigest          string    `json:"facts_digest"`
	}
	if binding.PolicyVersion != 2 || event.PolicyVersion != 2 || event.TargetResourceType != "ENTITLEMENT_PACKAGE" ||
		json.Unmarshal(command.NormalizedCommand, &normalized) != nil || json.Unmarshal(command.CriticalResourceVersions, &critical) != nil ||
		normalized.PackageID == uuid.Nil || normalized.PackageID.String() != event.TargetResourceID || critical.PackageID != normalized.PackageID || critical.PaymentOrderID != normalized.PaymentOrderID || critical.PolicyVersion != 2 ||
		!validIndependentApproval(command.RequesterUserID, command.ApproverUserID, command.DecisionReason) {
		return definiteAuthorizationFailure("COMMAND_SNAPSHOT_MISMATCH")
	}
	frozen := entitlementCorrectionFrozenFacts{WalletID: critical.WalletID, WalletVersion: critical.WalletVersion, PaymentTransactionID: critical.PaymentTransactionID,
		SourceComponentID: critical.SourceComponentID, SourceLotID: critical.SourceLotID, TargetComponentID: critical.TargetComponentID, TargetLotID: critical.TargetLotID,
		SourceType: critical.SourceType, TargetType: critical.TargetType, AmountMicrocredits: critical.AmountMicrocredits, SourceEvidenceDigest: critical.SourceEvidenceDigest, FactsDigest: critical.FactsDigest}
	result, err := domain.repository.ExecuteEntitlementComponentCorrection(ctx, binding.ApprovalRequestID, binding.EventID, binding.ApprovalEpoch,
		normalized.PackageID, normalized.PaymentOrderID, command.RequesterUserID, command.ApproverUserID, command.ExecutionIdempotencyKey, command.DecisionReason, normalized.ReasonCode, frozen)
	if err != nil {
		return classifyAuthorizationDomainFailure(err)
	}
	encoded, ledger, err := entitlementAuthorizationResult(result)
	if err != nil {
		return authorizationDomainAttempt{}
	}
	return authorizationDomainAttempt{Result: authorizationDomainResult{DomainResult: encoded, LedgerReferences: ledger}}
}
