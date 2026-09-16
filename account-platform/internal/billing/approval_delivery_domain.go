package billing

import (
	"context"
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
}

type billingAuthorizationExecutionRepository interface {
	approvalAdjustmentRepository
	providerResumeVersions(context.Context, ProviderResumeVersions) (ProviderResumeVersions, error)
	productResumeVersions(context.Context, ProductResumeVersions) (ProductResumeVersions, error)
	executeProviderResume(context.Context, ProviderResumeVersions, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, string, PaymentOrder, ProviderOrder) (ProviderResumeResult, error)
	executeProductResume(context.Context, ProductResumeVersions, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, string, PaymentOrder, ProviderOrder) (ProductResumeResult, error)
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
	default:
		return authorizationDomainResult{}, false, ErrConflict
	}
}

func (domain *billingAuthorizationDomain) ExecuteAuthorizationAttempt(ctx context.Context, binding ApprovalAuthorizationBinding, event approvalencoding.AuthorizationEvent, command ApprovedCommand) authorizationDomainAttempt {
	if domain == nil || domain.repository == nil {
		return authorizationDomainAttempt{}
	}
	if binding.SerializationVersion == approvalencoding.ApprovalSerializationVersion {
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
	artifacts, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{
		CommandType: event.CommandType, PolicyVersion: event.PolicyVersion, SnapshotSchemaVersion: 1,
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
	default:
		return false
	}
}
