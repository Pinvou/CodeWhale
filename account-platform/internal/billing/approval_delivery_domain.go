package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type billingAuthorizationDomain struct {
	repository billingAuthorizationExecutionRepository
	database   queryable
	payments   *PaymentCoordinator
	probe      func(context.Context, uuid.UUID) (PaymentOrder, ProviderOrder, uuid.UUID, error)
	proof      func(context.Context, approvalencoding.ProofRequest) (approvalencoding.ProofResponse, error)
	claim      func(context.Context, ApprovalAuthorizationBinding, approvalencoding.AuthorizationEvent) error
	priorClaim func(context.Context, ApprovalAuthorizationBinding) (bool, error)
}

type billingAuthorizationExecutionRepository interface {
	approvalAdjustmentRepository
	ApprovalCommandActivations(context.Context) (ApprovalCommandActivationSnapshot, error)
	providerResumeVersions(context.Context, ProviderResumeVersions) (ProviderResumeVersions, error)
	productResumeVersions(context.Context, ProductResumeVersions) (ProductResumeVersions, error)
	executeProviderResume(context.Context, ProviderResumeVersions, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, string, PaymentOrder, ProviderOrder) (ProviderResumeResult, error)
	executeProductResume(context.Context, ProductResumeVersions, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, string, PaymentOrder, ProviderOrder) (ProductResumeResult, error)
	EntitlementCorrectionForRequest(context.Context, uuid.UUID) (EntitlementCorrectionResult, error)
	ExecuteEntitlementComponentCorrection(context.Context, uuid.UUID, uuid.UUID, int64, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, string, string, string, entitlementCorrectionFrozenFacts) (EntitlementCorrectionResult, error)
	LedgerRepairForRequest(context.Context, uuid.UUID) (LedgerRepairResult, error)
	ApplyLedgerRepair(context.Context, LedgerRepairExecutionInput) (LedgerRepairResult, error)
	ReceivableWriteOffForRequest(context.Context, uuid.UUID) (ReceivableWriteOffResult, error)
	ApplyReceivableWriteOff(context.Context, ReceivableWriteOffExecutionInput) (ReceivableWriteOffResult, error)
	PaymentFinancialRepairForRequest(context.Context, uuid.UUID) (PaymentFinancialRepairResult, error)
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
		domain.payments = service.payments
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
	case ledgerRepairCommand:
		if domain.repository == nil {
			return authorizationDomainResult{}, false, ErrApprovalExecutionUnavailable
		}
		result, err := domain.repository.LedgerRepairForRequest(ctx, binding.ApprovalRequestID)
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
	case receivableWriteOffCommand:
		if domain.repository == nil {
			return authorizationDomainResult{}, false, ErrApprovalExecutionUnavailable
		}
		result, err := domain.repository.ReceivableWriteOffForRequest(ctx, binding.ApprovalRequestID)
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
	case paymentFinancialRepairCommand:
		if domain.repository == nil {
			return authorizationDomainResult{}, false, ErrApprovalExecutionUnavailable
		}
		result, err := domain.repository.PaymentFinancialRepairForRequest(ctx, binding.ApprovalRequestID)
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
	case ledgerRepairCommand:
		return domain.executeLedgerRepair(ctx, binding, event, command)
	case receivableWriteOffCommand:
		return domain.executeReceivableWriteOff(ctx, binding, event, command)
	case paymentFinancialRepairCommand:
		return domain.executePaymentFinancialRepair(ctx, binding, event, command)
	default:
		return authorizationDomainAttempt{StableFailureCode: "COMMAND_MAPPING_INVALID", FailureIsDefinite: true}
	}
}

// A proof-era event cannot reach a local effect (or a trusted provider probe)
// until its frozen command and current Billing owner revision agree. The
// command transactions still perform their own row/version CAS afterwards.

// approvalSnapshotSchemaVersion is the closed snapshot schema bound to a
// command's frozen proof input: the entitlement correction command owns the
// appended v2 schema, every other command keeps the original v1 schema.
func approvalSnapshotSchemaVersion(commandType string) int {
	if commandType == entitlementComponentAdjustmentCommand {
		return 2
	}
	return 1
}

func (domain *billingAuthorizationDomain) executionProof(ctx context.Context, binding ApprovalAuthorizationBinding, event approvalencoding.AuthorizationEvent, command ApprovedCommand) (authorizationDomainAttempt, bool) {
	if domain.proof == nil || event.ProofSchemaVersion != approvalencoding.ProofSchemaVersion ||
		event.ApprovalControlEpoch != binding.ApprovalEpoch || !sameRawJSON(command.NormalizedCommand, event.NormalizedCommand) ||
		!sameRawJSON(command.CriticalResourceVersions, event.CriticalResourceVersions) || !sameRawJSON(command.FinancialSnapshot, event.FinancialSnapshot) {
		return definiteAuthorizationFailure("COMMAND_SNAPSHOT_MISMATCH"), false
	}
	snapshotSchemaVersion := approvalSnapshotSchemaVersion(event.CommandType)
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

// executeLedgerRepair applies one consumed attribution repair. The approval
// snapshot carries only the wire-visible owner projection (target identifiers,
// entry type, amount and posting-set digest); the owner recomputes every fact
// and the facts digest again inside the execution transaction, so nothing the
// consumer presents can become a domain fact.
func (domain *billingAuthorizationDomain) executeLedgerRepair(ctx context.Context, binding ApprovalAuthorizationBinding,
	event approvalencoding.AuthorizationEvent, command ApprovedCommand) authorizationDomainAttempt {
	if domain.repository == nil {
		return authorizationDomainAttempt{}
	}
	var normalized struct {
		TargetJournalEntryID   uuid.UUID `json:"target_journal_entry_id"`
		TargetPostingID        uuid.UUID `json:"target_posting_id"`
		WrongLedgerAccountID   uuid.UUID `json:"wrong_ledger_account_id"`
		CorrectLedgerAccountID uuid.UUID `json:"correct_ledger_account_id"`
		AmountMicrocredits     int64     `json:"amount_microcredits"`
		ReasonCode             string    `json:"reason_code"`
	}
	var critical struct {
		TargetJournalEntryID uuid.UUID `json:"target_journal_entry_id"`
		TargetPostingID      uuid.UUID `json:"target_posting_id"`
		PostingSetDigest     string    `json:"posting_set_digest"`
		EntryType            string    `json:"entry_type"`
		PolicyVersion        int64     `json:"policy_version"`
	}
	var financial struct {
		AmountMicrocredits     int64      `json:"amount_microcredits"`
		WrongLedgerAccountID   uuid.UUID  `json:"wrong_ledger_account_id"`
		CorrectLedgerAccountID uuid.UUID  `json:"correct_ledger_account_id"`
		WrongAccountType       string     `json:"wrong_account_type"`
		CorrectAccountType     string     `json:"correct_account_type"`
		WalletID               *uuid.UUID `json:"wallet_id"`
	}
	if json.Unmarshal(event.NormalizedCommand, &normalized) != nil || json.Unmarshal(event.CriticalResourceVersions, &critical) != nil ||
		json.Unmarshal(event.FinancialSnapshot, &financial) != nil {
		return definiteAuthorizationFailure("COMMAND_SNAPSHOT_MISMATCH")
	}
	if normalized.TargetJournalEntryID == uuid.Nil || normalized.TargetPostingID == uuid.Nil ||
		normalized.WrongLedgerAccountID == uuid.Nil || normalized.CorrectLedgerAccountID == uuid.Nil ||
		normalized.WrongLedgerAccountID == normalized.CorrectLedgerAccountID || normalized.AmountMicrocredits < 1 ||
		strings.TrimSpace(normalized.ReasonCode) != normalized.ReasonCode || len(normalized.ReasonCode) == 0 || len(normalized.ReasonCode) > 80 {
		return definiteAuthorizationFailure("COMMAND_SNAPSHOT_MISMATCH")
	}
	if critical.TargetJournalEntryID != normalized.TargetJournalEntryID || critical.TargetPostingID != normalized.TargetPostingID ||
		critical.EntryType == "" || critical.PolicyVersion != 1 || !validHexDigest(critical.PostingSetDigest) ||
		financial.AmountMicrocredits != normalized.AmountMicrocredits ||
		financial.WrongLedgerAccountID != normalized.WrongLedgerAccountID ||
		financial.CorrectLedgerAccountID != normalized.CorrectLedgerAccountID ||
		financial.WrongAccountType == "" || financial.CorrectAccountType == "" ||
		financial.WrongAccountType == financial.CorrectAccountType || financial.WalletID != nil {
		return definiteAuthorizationFailure("COMMAND_SNAPSHOT_MISMATCH")
	}
	if event.CommandType != ledgerRepairCommand || event.TargetResourceType != "LEDGER_POSTING" ||
		event.TargetResourceID != normalized.TargetPostingID.String() ||
		command.TargetResourceType != event.TargetResourceType || command.TargetResourceID != event.TargetResourceID ||
		!sameRawJSON(event.NormalizedCommand, command.NormalizedCommand) ||
		!sameRawJSON(event.CriticalResourceVersions, command.CriticalResourceVersions) ||
		!sameRawJSON(event.FinancialSnapshot, command.FinancialSnapshot) ||
		event.ApprovalRequestID != binding.ApprovalRequestID || command.ApprovalRequestID != binding.ApprovalRequestID ||
		event.EventID != binding.EventID || event.ApprovalControlEpoch != binding.ApprovalEpoch ||
		event.ExecutorIdentity != billingApprovalExecutorIdentity || command.ExecutorIdentity != billingApprovalExecutorIdentity ||
		binding.ExecutorIdentity != billingApprovalExecutorIdentity ||
		event.PolicyVersion != 1 || command.PolicyVersion != 1 || binding.PolicyVersion != 1 ||
		!validIndependentApproval(command.RequesterUserID, command.ApproverUserID, command.DecisionReason) {
		return definiteAuthorizationFailure("COMMAND_SNAPSHOT_MISMATCH")
	}
	snapshot, err := domain.repository.ApprovalCommandActivations(ctx)
	if err != nil {
		return authorizationDomainAttempt{}
	}
	active, ok := findActiveAuthorizationCommand(snapshot, ledgerRepairCommand)
	if !ok || active.PolicyVersion != 1 || active.SnapshotSchemaVersion != 1 {
		// A command that is not ACTIVE at policy v1 / schema v1 can never
		// produce a domain effect; the consumer reports the frozen failure
		// classification instead of retrying until the attempt budget runs out.
		return definiteAuthorizationFailure("POLICY_OR_SCHEMA_MISMATCH")
	}
	result, err := domain.repository.ApplyLedgerRepair(ctx, LedgerRepairExecutionInput{
		ApprovalRequestID: binding.ApprovalRequestID, AuthorizationEventID: binding.EventID, ApprovalEpoch: binding.ApprovalEpoch,
		ExecutionKey: command.ExecutionIdempotencyKey,
		PolicyVersion: active.PolicyVersion, SchemaVersion: int64(active.SnapshotSchemaVersion), ActivationVersion: active.Version,
		Command: LedgerRepairCommandInput{
			TargetJournalEntryID: normalized.TargetJournalEntryID, TargetPostingID: normalized.TargetPostingID,
			WrongLedgerAccountID: normalized.WrongLedgerAccountID, CorrectLedgerAccountID: normalized.CorrectLedgerAccountID,
			AmountMicrocredits: normalized.AmountMicrocredits,
		},
		Frozen: LedgerRepairFrozenFacts{
			TargetJournalEntryID: normalized.TargetJournalEntryID, TargetPostingID: normalized.TargetPostingID,
			WrongLedgerAccountID: normalized.WrongLedgerAccountID, CorrectLedgerAccountID: normalized.CorrectLedgerAccountID,
			EntryType: critical.EntryType, AmountMicrocredits: financial.AmountMicrocredits,
			PostingSetDigest: critical.PostingSetDigest,
		},
	})
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

// executeReceivableWriteOff applies one consumed approved write-off. The
// approval snapshot carries only the wire-visible owner projection; the owner
// recomputes every fact and the facts digest again inside the execution
// transaction, so nothing the consumer presents can become a domain fact.
func (domain *billingAuthorizationDomain) executeReceivableWriteOff(ctx context.Context, binding ApprovalAuthorizationBinding,
	event approvalencoding.AuthorizationEvent, command ApprovedCommand) authorizationDomainAttempt {
	if domain.repository == nil {
		return authorizationDomainAttempt{}
	}
	var normalized struct {
		ReceivableID              uuid.UUID `json:"receivable_id"`
		WriteOffMicrocredits      int64     `json:"write_off_microcredits"`
		ExpectedReceivableVersion int64     `json:"expected_receivable_version"`
		ReasonCode                string    `json:"reason_code"`
	}
	var critical struct {
		ReceivableVersion int64  `json:"receivable_version"`
		FactsDigest       string `json:"facts_digest"`
		PolicyVersion     int64  `json:"policy_version"`
	}
	var financial struct {
		WriteOffMicrocredits         int64  `json:"write_off_microcredits"`
		OutstandingMicrocredits      int64  `json:"outstanding_microcredits"`
		SettledMicrocredits          int64  `json:"settled_microcredits"`
		OriginalMicrocredits         int64  `json:"original_microcredits"`
		SourceType                   string `json:"source_type"`
		SourceReference              string `json:"source_reference"`
		WalletReceivableMicrocredits int64  `json:"wallet_receivable_microcredits"`
	}
	if json.Unmarshal(event.NormalizedCommand, &normalized) != nil || json.Unmarshal(event.CriticalResourceVersions, &critical) != nil ||
		json.Unmarshal(event.FinancialSnapshot, &financial) != nil {
		return definiteAuthorizationFailure("COMMAND_SNAPSHOT_MISMATCH")
	}
	if normalized.ReceivableID == uuid.Nil || normalized.WriteOffMicrocredits < 1 || normalized.ExpectedReceivableVersion < 1 ||
		strings.TrimSpace(normalized.ReasonCode) != receivableWriteOffReason ||
		critical.ReceivableVersion != normalized.ExpectedReceivableVersion || critical.PolicyVersion != 1 || !validHexDigest(critical.FactsDigest) ||
		financial.WriteOffMicrocredits != normalized.WriteOffMicrocredits || financial.OutstandingMicrocredits < normalized.WriteOffMicrocredits ||
		financial.SettledMicrocredits < 0 || financial.OriginalMicrocredits < financial.OutstandingMicrocredits ||
		financial.SourceType != "USAGE_OVERAGE" || strings.TrimSpace(financial.SourceReference) == "" ||
		financial.WalletReceivableMicrocredits < normalized.WriteOffMicrocredits {
		return definiteAuthorizationFailure("COMMAND_SNAPSHOT_MISMATCH")
	}
	if event.CommandType != receivableWriteOffCommand || event.TargetResourceType != "RECEIVABLE" ||
		event.TargetResourceID != normalized.ReceivableID.String() ||
		command.TargetResourceType != event.TargetResourceType || command.TargetResourceID != event.TargetResourceID ||
		!sameRawJSON(event.NormalizedCommand, command.NormalizedCommand) ||
		!sameRawJSON(event.CriticalResourceVersions, command.CriticalResourceVersions) ||
		!sameRawJSON(event.FinancialSnapshot, command.FinancialSnapshot) ||
		event.ApprovalRequestID != binding.ApprovalRequestID || command.ApprovalRequestID != binding.ApprovalRequestID ||
		event.EventID != binding.EventID || event.ApprovalControlEpoch != binding.ApprovalEpoch ||
		event.ExecutorIdentity != billingApprovalExecutorIdentity || command.ExecutorIdentity != billingApprovalExecutorIdentity ||
		binding.ExecutorIdentity != billingApprovalExecutorIdentity ||
		event.PolicyVersion != 1 || command.PolicyVersion != 1 || binding.PolicyVersion != 1 ||
		!validIndependentApproval(command.RequesterUserID, command.ApproverUserID, command.DecisionReason) {
		return definiteAuthorizationFailure("COMMAND_SNAPSHOT_MISMATCH")
	}
	snapshot, err := domain.repository.ApprovalCommandActivations(ctx)
	if err != nil {
		return authorizationDomainAttempt{}
	}
	active, ok := findActiveAuthorizationCommand(snapshot, receivableWriteOffCommand)
	if !ok || active.PolicyVersion != 1 || active.SnapshotSchemaVersion != 1 {
		// A command that is not ACTIVE at policy v1 / schema v1 can never
		// produce a domain effect; the consumer reports the frozen failure
		// classification instead of retrying until the attempt budget runs out.
		return definiteAuthorizationFailure("POLICY_OR_SCHEMA_MISMATCH")
	}
	result, err := domain.repository.ApplyReceivableWriteOff(ctx, ReceivableWriteOffExecutionInput{
		ApprovalRequestID: binding.ApprovalRequestID, AuthorizationEventID: binding.EventID, ApprovalEpoch: binding.ApprovalEpoch,
		ExecutionKey: command.ExecutionIdempotencyKey,
		PolicyVersion: active.PolicyVersion, SchemaVersion: int64(active.SnapshotSchemaVersion), ActivationVersion: active.Version,
		Command: ReceivableWriteOffCommandInput{
			ReceivableID: normalized.ReceivableID, WriteOffMicrocredits: normalized.WriteOffMicrocredits,
			ExpectedReceivableVersion: normalized.ExpectedReceivableVersion,
		},
		Frozen: ReceivableWriteOffFrozenFacts{
			ReceivableVersion: critical.ReceivableVersion, WriteOffMicrocredits: financial.WriteOffMicrocredits,
			OutstandingMicrocredits: financial.OutstandingMicrocredits, SettledMicrocredits: financial.SettledMicrocredits,
			OriginalMicrocredits: financial.OriginalMicrocredits, SourceType: financial.SourceType,
			SourceReference: financial.SourceReference, WalletReceivableMicrocredits: financial.WalletReceivableMicrocredits,
		},
	})
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

func findActiveAuthorizationCommand(snapshot ApprovalCommandActivationSnapshot, commandType string) (ApprovalCommandActivation, bool) {
	for _, item := range snapshot.Activations {
		if item.CommandType == commandType && item.State == "ACTIVE" {
			return item, true
		}
	}
	return ApprovalCommandActivation{}, false
}

// executePaymentFinancialRepair applies one consumed confirmed-payment repair.
// The approval snapshot carries only the wire-visible owner projection; the
// owner recomputes every fact and the facts digest again inside the execution
// transaction, and the reused writer never calls the provider.
func (domain *billingAuthorizationDomain) executePaymentFinancialRepair(ctx context.Context, binding ApprovalAuthorizationBinding,
	event approvalencoding.AuthorizationEvent, command ApprovedCommand) authorizationDomainAttempt {
	if domain.repository == nil || domain.payments == nil {
		return authorizationDomainAttempt{}
	}
	var normalized struct {
		PaymentOrderID    uuid.UUID `json:"payment_order_id"`
		PaymentEvidenceID uuid.UUID `json:"payment_evidence_id"`
		ReasonCode        string    `json:"reason_code"`
	}
	var critical struct {
		PaymentOrderUpdatedAt string `json:"payment_order_updated_at"`
		FactsDigest           string `json:"facts_digest"`
		PolicyVersion         int64  `json:"policy_version"`
	}
	var financial struct {
		AmountFen              int64  `json:"amount_fen"`
		CreditMicrocredits     int64  `json:"credit_microcredits"`
		ProviderTransactionID  string `json:"provider_transaction_id"`
		WalletID               string `json:"wallet_id"`
		WalletVersion          int64  `json:"wallet_version"`
		AppliedToReceivables   int64  `json:"applied_to_receivables"`
	}
	if json.Unmarshal(event.NormalizedCommand, &normalized) != nil || json.Unmarshal(event.CriticalResourceVersions, &critical) != nil ||
		json.Unmarshal(event.FinancialSnapshot, &financial) != nil {
		return definiteAuthorizationFailure("COMMAND_SNAPSHOT_MISMATCH")
	}
	orderUpdatedAt, updatedAtErr := time.Parse(time.RFC3339Nano, critical.PaymentOrderUpdatedAt)
	walletID, walletErr := uuid.Parse(strings.TrimSpace(financial.WalletID))
	if normalized.PaymentOrderID == uuid.Nil || normalized.PaymentEvidenceID == uuid.Nil ||
		strings.TrimSpace(normalized.ReasonCode) != paymentFinancialRepairReason || critical.PolicyVersion != 1 ||
		!validHexDigest(critical.FactsDigest) || updatedAtErr != nil || orderUpdatedAt.Location() != time.UTC ||
		financial.AmountFen < 1 || financial.CreditMicrocredits < 1 ||
		strings.TrimSpace(financial.ProviderTransactionID) != financial.ProviderTransactionID || financial.ProviderTransactionID == "" ||
		walletErr != nil || walletID == uuid.Nil || financial.WalletVersion < 1 ||
		financial.AppliedToReceivables < 0 || financial.AppliedToReceivables > financial.CreditMicrocredits {
		return definiteAuthorizationFailure("COMMAND_SNAPSHOT_MISMATCH")
	}
	if event.CommandType != paymentFinancialRepairCommand || event.TargetResourceType != "PAYMENT_ORDER" ||
		event.TargetResourceID != normalized.PaymentOrderID.String() ||
		command.TargetResourceType != event.TargetResourceType || command.TargetResourceID != event.TargetResourceID ||
		!sameRawJSON(event.NormalizedCommand, command.NormalizedCommand) ||
		!sameRawJSON(event.CriticalResourceVersions, command.CriticalResourceVersions) ||
		!sameRawJSON(event.FinancialSnapshot, command.FinancialSnapshot) ||
		event.ApprovalRequestID != binding.ApprovalRequestID || command.ApprovalRequestID != binding.ApprovalRequestID ||
		event.EventID != binding.EventID || event.ApprovalControlEpoch != binding.ApprovalEpoch ||
		event.ExecutorIdentity != billingApprovalExecutorIdentity || command.ExecutorIdentity != billingApprovalExecutorIdentity ||
		binding.ExecutorIdentity != billingApprovalExecutorIdentity ||
		event.PolicyVersion != 1 || command.PolicyVersion != 1 || binding.PolicyVersion != 1 ||
		!validIndependentApproval(command.RequesterUserID, command.ApproverUserID, command.DecisionReason) {
		return definiteAuthorizationFailure("COMMAND_SNAPSHOT_MISMATCH")
	}
	snapshot, err := domain.repository.ApprovalCommandActivations(ctx)
	if err != nil {
		return authorizationDomainAttempt{}
	}
	active, ok := findActiveAuthorizationCommand(snapshot, paymentFinancialRepairCommand)
	if !ok || active.PolicyVersion != 1 || active.SnapshotSchemaVersion != 1 {
		return definiteAuthorizationFailure("POLICY_OR_SCHEMA_MISMATCH")
	}
	result, err := domain.payments.ApplyPaymentFinancialRepair(ctx, PaymentFinancialRepairExecutionInput{
		ApprovalRequestID: binding.ApprovalRequestID, AuthorizationEventID: binding.EventID, ApprovalEpoch: binding.ApprovalEpoch,
		ExecutionKey: command.ExecutionIdempotencyKey,
		PolicyVersion: active.PolicyVersion, SchemaVersion: int64(active.SnapshotSchemaVersion), ActivationVersion: active.Version,
		Command: PaymentFinancialRepairCommandInput{
			PaymentOrderID: normalized.PaymentOrderID, PaymentEvidenceID: normalized.PaymentEvidenceID,
		},
		Frozen: PaymentFinancialRepairFrozenFacts{
			OrderUpdatedAt: orderUpdatedAt, AmountFen: financial.AmountFen,
			CreditMicrocredits: financial.CreditMicrocredits, ProviderTransactionID: financial.ProviderTransactionID,
			WalletID: walletID, WalletVersion: financial.WalletVersion,
			AppliedToReceivables: financial.AppliedToReceivables,
		},
	})
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
	case ledgerRepairCommand:
		// A ledger repair targets one immutable ledger posting; the journal id
		// travels inside the normalized command, so Account binds the posting.
		return event.TargetResourceType == "LEDGER_POSTING" && strings.TrimSpace(event.TargetResourceID) == event.TargetResourceID
	case receivableWriteOffCommand:
		return event.TargetResourceType == "RECEIVABLE" && strings.TrimSpace(event.TargetResourceID) == event.TargetResourceID
	case paymentFinancialRepairCommand:
		return event.TargetResourceType == "PAYMENT_ORDER" && strings.TrimSpace(event.TargetResourceID) == event.TargetResourceID
	default:
		return false
	}
}
