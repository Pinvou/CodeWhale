package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type billingAuthorizationDomain struct {
	repository billingAuthorizationExecutionRepository
	database   queryable
	probe      func(context.Context, uuid.UUID) (PaymentOrder, ProviderOrder, uuid.UUID, error)
	proof      func(context.Context, approvalencoding.ProofRequest) (approvalencoding.ProofResponse, error)
	claim      func(context.Context, ApprovalAuthorizationBinding, approvalencoding.AuthorizationEvent) error
	priorClaim func(context.Context, ApprovalAuthorizationBinding) (bool, error)
}

type billingAuthorizationExecutionRepository interface {
	approvalAdjustmentRepository
	providerResumeVersions(context.Context, ProviderResumeVersions) (ProviderResumeVersions, error)
	productResumeVersions(context.Context, ProductResumeVersions) (ProductResumeVersions, error)
	executeProviderResume(context.Context, ProviderResumeVersions, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, string, PaymentOrder, ProviderOrder) (ProviderResumeResult, error)
	executeProductResume(context.Context, ProductResumeVersions, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, string, PaymentOrder, ProviderOrder) (ProductResumeResult, error)
	EntitlementCorrectionForRequest(context.Context, uuid.UUID) (EntitlementCorrectionResult, error)
	ExecuteEntitlementComponentCorrection(context.Context, uuid.UUID, uuid.UUID, int64, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, string, string, string, entitlementCorrectionFrozenFacts) (EntitlementCorrectionResult, error)
}

func newBillingAuthorizationDomain(service *Service) approvalAuthorizationDomain {
	if service == nil {
		return nil
	}
	repository, _ := service.repository.(*PostgresRepository)
	if repository == nil {
		if adapter, ok := service.repository.(approvalAuthorizationDomain); ok {
			return adapter
		}
	}
	domain := &billingAuthorizationDomain{repository: repository}
	domain.proof = service.billingApprovalProof
	if repository != nil {
		domain.database = repository.pool
		domain.claim = repository.ClaimBillingApprovalProof
		domain.priorClaim = repository.ExistingBillingApprovalProofClaim
	}
	if service.payments != nil {
		domain.probe = service.payments.probeProviderRecovery
	}
	return domain
}

func (domain *billingAuthorizationDomain) LookupAuthorizationResult(ctx context.Context, binding ApprovalAuthorizationBinding, event approvalencoding.AuthorizationEvent) (authorizationDomainResult, bool, error) {
	if domain == nil || domain.database == nil {
		return authorizationDomainResult{}, false, ErrApprovalExecutionUnavailable
	}
	switch binding.CommandType {
	case "WALLET_ADJUSTMENT":
		var adjustmentID uuid.UUID
		err := domain.database.QueryRow(ctx, `SELECT id FROM manual_adjustments WHERE approval_request_id=$1 AND status='POSTED'`, binding.ApprovalRequestID).Scan(&adjustmentID)
		if errors.Is(err, pgx.ErrNoRows) {
			return authorizationDomainResult{}, false, nil
		}
		if err != nil {
			return authorizationDomainResult{}, false, err
		}
		adjustment, err := getAdjustment(ctx, domain.database, adjustmentID, false)
		if err != nil {
			return authorizationDomainResult{}, false, err
		}
		result, ledger := adjustmentAuthorizationResult(adjustment)
		return authorizationDomainResult{DomainResult: result, LedgerReferences: ledger}, true, nil
	case "PAYMENT_PROVIDER_RESUME":
		var result ProviderResumeResult
		err := domain.database.QueryRow(ctx, `SELECT id,provider_id,'RESUMED' FROM payment_provider_resume_executions WHERE approval_request_id=$1`, binding.ApprovalRequestID).Scan(&result.ExecutionID, &result.ProviderID, &result.Status)
		if errors.Is(err, pgx.ErrNoRows) {
			return authorizationDomainResult{}, false, nil
		}
		if err != nil {
			return authorizationDomainResult{}, false, err
		}
		encoded, err := json.Marshal(result)
		return authorizationDomainResult{DomainResult: encoded, LedgerReferences: json.RawMessage(`{}`)}, true, err
	case "PAYMENT_PRODUCT_RESUME":
		var result ProductResumeResult
		err := domain.database.QueryRow(ctx, `SELECT id,provider_id,product_id,'RESUMED' FROM payment_product_resume_executions WHERE approval_request_id=$1`, binding.ApprovalRequestID).Scan(&result.ExecutionID, &result.ProviderID, &result.ProductID, &result.Status)
		if errors.Is(err, pgx.ErrNoRows) {
			return authorizationDomainResult{}, false, nil
		}
		if err != nil {
			return authorizationDomainResult{}, false, err
		}
		encoded, err := json.Marshal(result)
		return authorizationDomainResult{DomainResult: encoded, LedgerReferences: json.RawMessage(`{}`)}, true, err
	case entitlementComponentAdjustmentCommand:
		if domain.repository == nil {
			return authorizationDomainResult{}, false, ErrApprovalExecutionUnavailable
		}
		result, err := domain.repository.EntitlementCorrectionForRequest(ctx, binding.ApprovalRequestID)
		if errors.Is(err, ErrNotFound) {
			return authorizationDomainResult{}, false, nil
		}
		if err != nil {
			return authorizationDomainResult{}, false, err
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return authorizationDomainResult{}, false, err
		}
		ledger, _ := json.Marshal(map[string]any{"journal_entry_id": result.JournalEntryID})
		return authorizationDomainResult{DomainResult: encoded, LedgerReferences: ledger}, true, nil
	default:
		return authorizationDomainResult{}, false, ErrConflict
	}
}

func (domain *billingAuthorizationDomain) ExecuteAuthorizationAttempt(ctx context.Context, binding ApprovalAuthorizationBinding, event approvalencoding.AuthorizationEvent, command ApprovedCommand) authorizationDomainAttempt {
	if domain == nil || domain.repository == nil {
		return authorizationDomainAttempt{}
	}
	if binding.SerializationVersion == approvalencoding.ApprovalSerializationVersion {
		if domain.priorClaim == nil {
			return authorizationDomainAttempt{}
		}
		claimed, err := domain.priorClaim(ctx, binding)
		if err != nil || claimed {
			return authorizationDomainAttempt{}
		}
		if result, valid := domain.executionProof(ctx, binding, event, command); !valid {
			return result
		}
		if domain.claim == nil {
			return authorizationDomainAttempt{}
		}
		if err := domain.claim(ctx, binding, event); err != nil {
			if errors.Is(err, ErrApprovalSnapshotMismatch) {
				return definiteAuthorizationFailure("DOMAIN_REVISION_CHANGED")
			}
			return authorizationDomainAttempt{}
		}
	}
	switch binding.CommandType {
	case "WALLET_ADJUSTMENT":
		return domain.executeAdjustment(ctx, binding, event, command)
	case "PAYMENT_PROVIDER_RESUME":
		return domain.executeProviderResume(ctx, binding, event, command)
	case "PAYMENT_PRODUCT_RESUME":
		return domain.executeProductResume(ctx, binding, event, command)
	case entitlementComponentAdjustmentCommand:
		return domain.executeEntitlementCorrection(ctx, binding, event, command)
	default:
		return authorizationDomainAttempt{StableFailureCode: "COMMAND_MAPPING_INVALID", FailureIsDefinite: true}
	}
}

// A proof-era event cannot reach a local effect (or a trusted provider probe)
// until its frozen command and current Billing owner revision agree. The
// command transactions still perform their own row/version CAS afterwards.
func (domain *billingAuthorizationDomain) executionProof(ctx context.Context, binding ApprovalAuthorizationBinding, event approvalencoding.AuthorizationEvent, command ApprovedCommand) (authorizationDomainAttempt, bool) {
	if domain.proof == nil || event.ProofSchemaVersion != approvalencoding.ProofSchemaVersion ||
		event.ApprovalControlEpoch != binding.ApprovalEpoch || !sameRawJSON(command.NormalizedCommand, event.NormalizedCommand) ||
		!sameRawJSON(command.CriticalResourceVersions, event.CriticalResourceVersions) || !sameRawJSON(command.FinancialSnapshot, event.FinancialSnapshot) {
		return definiteAuthorizationFailure("COMMAND_SNAPSHOT_MISMATCH"), false
	}
	snapshotSchemaVersion := 1
	if event.CommandType == entitlementComponentAdjustmentCommand {
		snapshotSchemaVersion = 2
	}
	artifacts, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{
		CommandType: event.CommandType, PolicyVersion: event.PolicyVersion, SnapshotSchemaVersion: snapshotSchemaVersion,
		TargetResourceType: event.TargetResourceType, TargetResourceID: event.TargetResourceID,
		NormalizedCommand: event.NormalizedCommand, CriticalResourceVersions: event.CriticalResourceVersions,
		FinancialSnapshot: event.FinancialSnapshot,
	})
	if err != nil || artifacts.CommandDigest != event.CommandDigest || artifacts.SnapshotDigest != event.SnapshotDigest {
		return definiteAuthorizationFailure("COMMAND_SNAPSHOT_MISMATCH"), false
	}
	proof, err := domain.proof(ctx, approvalencoding.ProofRequest{ApprovalRequestID: binding.ApprovalRequestID, Artifacts: artifacts})
	if err != nil {
		return authorizationDomainAttempt{}, false
	}
	if proof.Verdict != "ALLOW" || proof.DomainRevision != event.DomainRevision || proof.CommandDigest != event.CommandDigest || proof.SnapshotDigest != event.SnapshotDigest {
		return definiteAuthorizationFailure("DOMAIN_REVISION_CHANGED"), false
	}
	return authorizationDomainAttempt{}, true
}

func (domain *billingAuthorizationDomain) executeAdjustment(ctx context.Context, binding ApprovalAuthorizationBinding, event approvalencoding.AuthorizationEvent, command ApprovedCommand) authorizationDomainAttempt {
	adjustmentID, err := uuid.Parse(event.TargetResourceID)
	if err != nil || adjustmentID == uuid.Nil {
		return definiteAuthorizationFailure("COMMAND_SNAPSHOT_MISMATCH")
	}
	adjustment, err := domain.repository.AdjustmentForExecution(ctx, adjustmentID)
	if err != nil {
		return classifyAuthorizationDomainFailure(err)
	}
	if !sameRawJSON(event.CriticalResourceVersions, command.CriticalResourceVersions) || validateApprovedAdjustmentCommand(command, adjustment) != nil {
		return definiteAuthorizationFailure("COMMAND_SNAPSHOT_MISMATCH")
	}
	posted, err := domain.repository.ExecuteApprovedAdjustment(ctx, adjustmentID, binding.ApprovalRequestID, command.ExecutionIdempotencyKey, command.ApproverUserID, command.DecisionReason)
	if err != nil {
		return classifyAuthorizationDomainFailure(err)
	}
	result, ledger := adjustmentAuthorizationResult(posted)
	return authorizationDomainAttempt{Result: authorizationDomainResult{DomainResult: result, LedgerReferences: ledger}}
}

func (domain *billingAuthorizationDomain) executeEntitlementCorrection(ctx context.Context, binding ApprovalAuthorizationBinding,
	event approvalencoding.AuthorizationEvent, command ApprovedCommand) authorizationDomainAttempt {
	var normalized struct {
		PackageID      uuid.UUID `json:"package_id"`
		PaymentOrderID uuid.UUID `json:"payment_order_id"`
		ReasonCode     string    `json:"reason_code"`
	}
	var critical struct {
		PackageID            uuid.UUID  `json:"package_id"`
		WalletID             uuid.UUID  `json:"wallet_id"`
		WalletVersion        int64      `json:"wallet_version"`
		PaymentOrderID       uuid.UUID  `json:"payment_order_id"`
		PaymentTransactionID uuid.UUID  `json:"payment_transaction_id"`
		SourceComponentID    uuid.UUID  `json:"source_component_id"`
		SourceLotID          uuid.UUID  `json:"source_lot_id"`
		TargetComponentID    *uuid.UUID `json:"target_component_id"`
		TargetLotID          *uuid.UUID `json:"target_lot_id"`
		SourceType           string     `json:"source_type"`
		TargetType           string     `json:"target_type"`
		AmountMicrocredits   int64      `json:"amount_microcredits"`
		SourceEvidenceDigest string     `json:"source_evidence_digest"`
		FactsDigest          string     `json:"facts_digest"`
		PolicyVersion        int64      `json:"policy_version"`
	}
	if json.Unmarshal(event.NormalizedCommand, &normalized) != nil || json.Unmarshal(event.CriticalResourceVersions, &critical) != nil ||
		normalized.PackageID == uuid.Nil || normalized.PaymentOrderID == uuid.Nil ||
		strings.TrimSpace(normalized.ReasonCode) != normalized.ReasonCode || len(normalized.ReasonCode) == 0 || len(normalized.ReasonCode) > 80 ||
		event.CommandType != entitlementComponentAdjustmentCommand || event.TargetResourceType != "ENTITLEMENT_PACKAGE" ||
		event.TargetResourceID != normalized.PackageID.String() || command.TargetResourceType != event.TargetResourceType ||
		command.TargetResourceID != event.TargetResourceID || !sameRawJSON(event.NormalizedCommand, command.NormalizedCommand) ||
		!sameRawJSON(event.CriticalResourceVersions, command.CriticalResourceVersions) ||
		!sameRawJSON(event.FinancialSnapshot, command.FinancialSnapshot) ||
		event.ApprovalRequestID != binding.ApprovalRequestID || command.ApprovalRequestID != binding.ApprovalRequestID ||
		event.EventID != binding.EventID || event.ApprovalControlEpoch != binding.ApprovalEpoch ||
		event.ExecutorIdentity != billingApprovalExecutorIdentity || command.ExecutorIdentity != billingApprovalExecutorIdentity ||
		binding.ExecutorIdentity != billingApprovalExecutorIdentity ||
		!validIndependentApproval(command.RequesterUserID, command.ApproverUserID, command.DecisionReason) {
		return definiteAuthorizationFailure("COMMAND_SNAPSHOT_MISMATCH")
	}
	targetComponentID, targetLotID := uuid.Nil, uuid.Nil
	if critical.TargetComponentID != nil {
		targetComponentID = *critical.TargetComponentID
	}
	if critical.TargetLotID != nil {
		targetLotID = *critical.TargetLotID
	}
	frozen := entitlementCorrectionFrozenFacts{
		WalletID: critical.WalletID, WalletVersion: critical.WalletVersion, PaymentTransactionID: critical.PaymentTransactionID,
		SourceComponentID: critical.SourceComponentID, SourceLotID: critical.SourceLotID,
		TargetComponentID: targetComponentID, TargetLotID: targetLotID,
		SourceType: critical.SourceType, TargetType: critical.TargetType, AmountMicrocredits: critical.AmountMicrocredits,
		SourceEvidenceDigest: critical.SourceEvidenceDigest, FactsDigest: critical.FactsDigest,
	}
	validTypes := (frozen.SourceType == "BASE" && frozen.TargetType == "BONUS") ||
		(frozen.SourceType == "BONUS" && frozen.TargetType == "BASE")
	if critical.PackageID != normalized.PackageID || critical.PaymentOrderID != normalized.PaymentOrderID ||
		critical.PolicyVersion != event.PolicyVersion || event.PolicyVersion != 2 || command.PolicyVersion != 2 ||
		binding.PolicyVersion != 2 || frozen.WalletID == uuid.Nil || frozen.WalletVersion < 1 ||
		frozen.PaymentTransactionID == uuid.Nil || frozen.SourceComponentID == uuid.Nil || frozen.SourceLotID == uuid.Nil ||
		(frozen.TargetComponentID == uuid.Nil) != (frozen.TargetLotID == uuid.Nil) || !validTypes ||
		frozen.AmountMicrocredits < 1 || !validHexDigest(frozen.SourceEvidenceDigest) || !validHexDigest(frozen.FactsDigest) {
		return definiteAuthorizationFailure("COMMAND_SNAPSHOT_MISMATCH")
	}
	result, err := domain.repository.ExecuteEntitlementComponentCorrection(ctx, binding.ApprovalRequestID, binding.EventID,
		binding.ApprovalEpoch, normalized.PackageID, normalized.PaymentOrderID, command.RequesterUserID, command.ApproverUserID,
		command.ExecutionIdempotencyKey, command.DecisionReason, normalized.ReasonCode, frozen)
	if err != nil {
		return classifyAuthorizationDomainFailure(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return authorizationDomainAttempt{}
	}
	ledger, _ := json.Marshal(map[string]any{"journal_entry_id": result.JournalEntryID})
	return authorizationDomainAttempt{Result: authorizationDomainResult{DomainResult: encoded, LedgerReferences: ledger}}
}

func (domain *billingAuthorizationDomain) executeProviderResume(ctx context.Context, binding ApprovalAuthorizationBinding, event approvalencoding.AuthorizationEvent, command ApprovedCommand) authorizationDomainAttempt {
	if domain.probe == nil {
		return authorizationDomainAttempt{}
	}
	var observed, normalized ProviderResumeVersions
	if json.Unmarshal(event.CriticalResourceVersions, &observed) != nil || json.Unmarshal(command.NormalizedCommand, &normalized) != nil ||
		event.TargetResourceType != "PAYMENT_PROVIDER_CONTROL" || event.TargetResourceID != observed.ProviderID ||
		!sameProviderResumeVersions(normalized, observed) || !validIndependentApproval(command.RequesterUserID, command.ApproverUserID, command.DecisionReason) {
		return definiteAuthorizationFailure("COMMAND_SNAPSHOT_MISMATCH")
	}
	actual, err := domain.repository.providerResumeVersions(ctx, observed)
	if err != nil {
		return classifyAuthorizationDomainFailure(err)
	}
	if !sameProviderResumeVersions(actual, observed) {
		return definiteAuthorizationFailure("COMMAND_SNAPSHOT_MISMATCH")
	}
	// The trusted provider probe persists an observation and is therefore part
	// of the granted attempt. Any error from this point onward is conservative
	// UNKNOWN, even when no resume execution row is currently visible.
	order, providerOrder, observationID, err := domain.probe(ctx, observed.ProbeOrderID)
	if err != nil {
		return authorizationDomainAttempt{}
	}
	result, err := domain.repository.executeProviderResume(ctx, observed, observationID, binding.ApprovalRequestID, command.RequesterUserID, command.ApproverUserID, command.DecisionReason, order, providerOrder)
	if err != nil {
		return authorizationDomainAttempt{}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return authorizationDomainAttempt{}
	}
	return authorizationDomainAttempt{Result: authorizationDomainResult{DomainResult: encoded, LedgerReferences: json.RawMessage(`{}`)}}
}

func (domain *billingAuthorizationDomain) executeProductResume(ctx context.Context, binding ApprovalAuthorizationBinding, event approvalencoding.AuthorizationEvent, command ApprovedCommand) authorizationDomainAttempt {
	if domain.probe == nil {
		return authorizationDomainAttempt{}
	}
	var observed, normalized ProductResumeVersions
	if json.Unmarshal(event.CriticalResourceVersions, &observed) != nil || json.Unmarshal(command.NormalizedCommand, &normalized) != nil ||
		event.TargetResourceType != "PAYMENT_PRODUCT_CONTROL" || event.TargetResourceID != observed.ProductID ||
		!sameProductResumeVersions(normalized, observed) || !validIndependentApproval(command.RequesterUserID, command.ApproverUserID, command.DecisionReason) {
		return definiteAuthorizationFailure("COMMAND_SNAPSHOT_MISMATCH")
	}
	actual, err := domain.repository.productResumeVersions(ctx, observed)
	if err != nil {
		return classifyAuthorizationDomainFailure(err)
	}
	if !sameProductResumeVersions(actual, observed) {
		return definiteAuthorizationFailure("COMMAND_SNAPSHOT_MISMATCH")
	}
	order, providerOrder, observationID, err := domain.probe(ctx, observed.ProbeOrderID)
	if err != nil || order.ProductID != observed.ProductID {
		return authorizationDomainAttempt{}
	}
	result, err := domain.repository.executeProductResume(ctx, observed, observationID, binding.ApprovalRequestID, command.RequesterUserID, command.ApproverUserID, command.DecisionReason, order, providerOrder)
	if err != nil {
		return authorizationDomainAttempt{}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return authorizationDomainAttempt{}
	}
	return authorizationDomainAttempt{Result: authorizationDomainResult{DomainResult: encoded, LedgerReferences: json.RawMessage(`{}`)}}
}

func adjustmentAuthorizationResult(adjustment ManualAdjustment) (json.RawMessage, json.RawMessage) {
	result, _ := json.Marshal(map[string]any{"adjustment_id": adjustment.ID, "wallet_id": adjustment.WalletID, "status": adjustment.Status, "version": adjustment.Version})
	ledger := json.RawMessage(`{}`)
	if adjustment.JournalEntryID != nil {
		ledger, _ = json.Marshal(map[string]any{"journal_entry_id": adjustment.JournalEntryID})
	}
	return result, ledger
}

func definiteAuthorizationFailure(code string) authorizationDomainAttempt {
	return authorizationDomainAttempt{StableFailureCode: code, FailureIsDefinite: true}
}

func validHexDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func classifyAuthorizationDomainFailure(err error) authorizationDomainAttempt {
	switch {
	case errors.Is(err, ErrNotFound):
		return definiteAuthorizationFailure("DOMAIN_RESOURCE_NOT_FOUND")
	case errors.Is(err, ErrApprovalSnapshotMismatch), errors.Is(err, ErrConflict), errors.Is(err, ErrInsufficientBalance), errors.Is(err, ErrWalletUnavailable):
		return definiteAuthorizationFailure("DOMAIN_PRECONDITION_REJECTED")
	default:
		return authorizationDomainAttempt{}
	}
}

func validAuthorizationCommandTarget(event approvalencoding.AuthorizationEvent) bool {
	switch event.CommandType {
	case "WALLET_ADJUSTMENT":
		return event.TargetResourceType == "MANUAL_ADJUSTMENT" && strings.TrimSpace(event.TargetResourceID) == event.TargetResourceID
	case "PAYMENT_PROVIDER_RESUME":
		return event.TargetResourceType == "PAYMENT_PROVIDER_CONTROL" && strings.TrimSpace(event.TargetResourceID) == event.TargetResourceID
	case "PAYMENT_PRODUCT_RESUME":
		return event.TargetResourceType == "PAYMENT_PRODUCT_CONTROL" && strings.TrimSpace(event.TargetResourceID) == event.TargetResourceID
	case entitlementComponentAdjustmentCommand:
		return event.TargetResourceType == "ENTITLEMENT_PACKAGE" && strings.TrimSpace(event.TargetResourceID) == event.TargetResourceID
	default:
		return false
	}
}
