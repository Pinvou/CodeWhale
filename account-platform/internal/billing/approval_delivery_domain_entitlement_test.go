package billing

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
)

func entitlementCorrectionCommandFixture() (ApprovalAuthorizationBinding, approvalencoding.AuthorizationEvent, ApprovedCommand, entitlementCorrectionFrozenFacts) {
	packageID, orderID, walletID, transactionID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	sourceComponentID, sourceLotID := uuid.New(), uuid.New()
	requesterID, approverID := uuid.New(), uuid.New()
	digest := strings.Repeat("a", 64)
	normalized, _ := json.Marshal(map[string]any{
		"package_id": packageID, "payment_order_id": orderID, "reason_code": "ORIGINAL_SPLIT_MISMATCH",
	})
	critical, _ := json.Marshal(map[string]any{
		"package_id": packageID, "wallet_id": walletID, "wallet_version": 3,
		"payment_order_id": orderID, "payment_transaction_id": transactionID,
		"source_component_id": sourceComponentID, "source_lot_id": sourceLotID,
		"target_component_id": nil, "target_lot_id": nil,
		"source_type": "BASE", "target_type": "BONUS", "amount_microcredits": 20,
		"source_evidence_digest": digest, "facts_digest": strings.Repeat("b", 64), "policy_version": 2,
	})
	financial, _ := json.Marshal(map[string]any{
		"package_total_microcredits": 100, "source_original_microcredits": 100,
		"target_original_microcredits": 0, "effective_total_microcredits": 100,
	})
	binding := ApprovalAuthorizationBinding{
		ApprovalRequestID: uuid.New(), ApprovalEpoch: 2, PolicyVersion: 2, CommandType: entitlementComponentAdjustmentCommand,
		EventID: uuid.New(), ExecutorIdentity: billingApprovalExecutorIdentity,
	}
	event := approvalencoding.AuthorizationEvent{
		CommandType: entitlementComponentAdjustmentCommand, PolicyVersion: 2,
		EventID: binding.EventID, ApprovalRequestID: binding.ApprovalRequestID,
		ExecutorIdentity: billingApprovalExecutorIdentity, ApprovalControlEpoch: binding.ApprovalEpoch,
		TargetResourceType: "ENTITLEMENT_PACKAGE", TargetResourceID: packageID.String(),
		NormalizedCommand: normalized, CriticalResourceVersions: critical, FinancialSnapshot: financial,
	}
	command := ApprovedCommand{
		CommandType: event.CommandType, PolicyVersion: 2, TargetResourceType: event.TargetResourceType,
		TargetResourceID: event.TargetResourceID, NormalizedCommand: normalized,
		CriticalResourceVersions: critical, FinancialSnapshot: financial,
		ApprovalRequestID: binding.ApprovalRequestID, ExecutorIdentity: billingApprovalExecutorIdentity,
		RequesterUserID: requesterID, ApproverUserID: approverID,
		DecisionReason: "independent review completed", ExecutionIdempotencyKey: "approval:entitlement:stable",
	}
	frozen := entitlementCorrectionFrozenFacts{
		WalletID: walletID, WalletVersion: 3, PaymentTransactionID: transactionID,
		SourceComponentID: sourceComponentID, SourceLotID: sourceLotID,
		TargetComponentID: uuid.Nil, TargetLotID: uuid.Nil,
		SourceType: "BASE", TargetType: "BONUS", AmountMicrocredits: 20,
		SourceEvidenceDigest: digest, FactsDigest: strings.Repeat("b", 64),
	}
	return binding, event, command, frozen
}

func TestBillingAuthorizationDomainEntitlementCorrectionMapping(t *testing.T) {
	binding, event, command, frozen := entitlementCorrectionCommandFixture()
	repository := &entitlementAuthorizationRepositoryStub{entitlementCorrection: EntitlementCorrectionResult{CorrectionID: uuid.New(), Status: "SUCCEEDED"}}
	domain := &billingAuthorizationDomain{repository: repository}

	attempt := domain.ExecuteAuthorizationAttempt(context.Background(), binding, event, command)
	if attempt.FailureIsDefinite || len(attempt.Result.DomainResult) == 0 || repository.entitlementRuns != 1 ||
		repository.entitlementFrozen != frozen || repository.entitlementKey != command.ExecutionIdempotencyKey ||
		repository.entitlementReasonCode != "ORIGINAL_SPLIT_MISMATCH" {
		t.Fatalf("entitlement mapping attempt=%+v runs=%d frozen=%+v", attempt, repository.entitlementRuns, repository.entitlementFrozen)
	}
	if !strings.Contains(string(attempt.Result.LedgerReferences), `"journal_entry_id"`) {
		t.Fatalf("entitlement ledger reference=%s", attempt.Result.LedgerReferences)
	}

	repository.entitlementRunErr = ErrConflict
	attempt = domain.ExecuteAuthorizationAttempt(context.Background(), binding, event, command)
	if !attempt.FailureIsDefinite || attempt.StableFailureCode != "DOMAIN_PRECONDITION_REJECTED" || repository.entitlementRuns != 2 {
		t.Fatalf("entitlement conflict classification=%+v runs=%d", attempt, repository.entitlementRuns)
	}
	repository.entitlementRunErr = errors.New("commit outcome unknown")
	attempt = domain.ExecuteAuthorizationAttempt(context.Background(), binding, event, command)
	if attempt.FailureIsDefinite || len(attempt.Result.DomainResult) != 0 || repository.entitlementRuns != 3 {
		t.Fatalf("entitlement unknown outcome=%+v runs=%d", attempt, repository.entitlementRuns)
	}
}

func TestBillingAuthorizationDomainEntitlementResultFirstRead(t *testing.T) {
	binding, event, _, _ := entitlementCorrectionCommandFixture()
	correction := EntitlementCorrectionResult{CorrectionID: uuid.New(), JournalEntryID: uuid.New(), Status: "SUCCEEDED"}
	domain := &billingAuthorizationDomain{database: &entitlementAuthorizationQueryStub{t: t}, repository: &entitlementAuthorizationRepositoryStub{entitlementCorrection: correction}}
	result, found, err := domain.LookupAuthorizationResult(context.Background(), binding, event)
	if err != nil || !found || !strings.Contains(string(result.DomainResult), correction.CorrectionID.String()) ||
		!strings.Contains(string(result.LedgerReferences), correction.JournalEntryID.String()) {
		t.Fatalf("result-first correction=%+v found=%v err=%v", result, found, err)
	}

	domain = &billingAuthorizationDomain{database: &entitlementAuthorizationQueryStub{t: t}, repository: &entitlementAuthorizationRepositoryStub{entitlementCorrectionErr: ErrNotFound}}
	result, found, err = domain.LookupAuthorizationResult(context.Background(), binding, event)
	if err != nil || found || len(result.DomainResult) != 0 {
		t.Fatalf("absent correction result=%+v found=%v err=%v", result, found, err)
	}
	domain = &billingAuthorizationDomain{database: &entitlementAuthorizationQueryStub{t: t}, repository: &entitlementAuthorizationRepositoryStub{entitlementCorrectionErr: errors.New("read unavailable")}}
	if _, found, err = domain.LookupAuthorizationResult(context.Background(), binding, event); err == nil || found {
		t.Fatalf("read error became absence: found=%v err=%v", found, err)
	}
}

func TestBillingAuthorizationDomainEntitlementProofUsesSchemaV2BeforeEffect(t *testing.T) {
	binding, event, command, _ := entitlementCorrectionCommandFixture()
	artifacts, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{
		CommandType: event.CommandType, PolicyVersion: 2, SnapshotSchemaVersion: 2,
		TargetResourceType: event.TargetResourceType, TargetResourceID: event.TargetResourceID,
		NormalizedCommand: event.NormalizedCommand, CriticalResourceVersions: event.CriticalResourceVersions,
		FinancialSnapshot: event.FinancialSnapshot,
	})
	if err != nil {
		t.Fatal(err)
	}
	event.ProofSchemaVersion = "1"
	event.CommandDigest = artifacts.CommandDigest
	event.SnapshotDigest = artifacts.SnapshotDigest
	event.DomainRevision = "1:" + strings.Repeat("c", 64)
	event.ApprovalControlEpoch = binding.ApprovalEpoch
	binding.SerializationVersion = approvalencoding.ApprovalSerializationVersion

	repository := &entitlementAuthorizationRepositoryStub{}
	proofCalls := 0
	domain := &billingAuthorizationDomain{
		repository: repository,
		priorClaim: func(context.Context, ApprovalAuthorizationBinding) (bool, error) { return false, nil },
		proof: func(_ context.Context, request approvalencoding.ProofRequest) (approvalencoding.ProofResponse, error) {
			proofCalls++
			if request.Artifacts.PolicyVersion != 2 || request.Artifacts.SnapshotSchemaVersion != 2 ||
				request.Artifacts.CommandDigest != event.CommandDigest || request.Artifacts.SnapshotDigest != event.SnapshotDigest {
				t.Fatalf("proof artifacts=%+v", request.Artifacts)
			}
			return approvalencoding.ProofResponse{Verdict: "ALLOW", DomainRevision: event.DomainRevision,
				CommandDigest: event.CommandDigest, SnapshotDigest: event.SnapshotDigest}, nil
		},
	}
	claims := 0
	domain.claim = func(context.Context, ApprovalAuthorizationBinding, approvalencoding.AuthorizationEvent) error {
		claims++
		return nil
	}
	attempt := domain.ExecuteAuthorizationAttempt(context.Background(), binding, event, command)
	if attempt.FailureIsDefinite || proofCalls != 1 || claims != 1 || repository.entitlementRuns != 1 {
		t.Fatalf("v2 proof path attempt=%+v proof=%d claims=%d runs=%d", attempt, proofCalls, claims, repository.entitlementRuns)
	}

	domain.proof = func(context.Context, approvalencoding.ProofRequest) (approvalencoding.ProofResponse, error) {
		proofCalls++
		return approvalencoding.ProofResponse{}, errors.New("proof unavailable")
	}
	attempt = domain.ExecuteAuthorizationAttempt(context.Background(), binding, event, command)
	if attempt.FailureIsDefinite || repository.entitlementRuns != 1 || proofCalls != 2 {
		t.Fatalf("proof failure crossed effect boundary: %+v runs=%d proof=%d", attempt, repository.entitlementRuns, proofCalls)
	}
}
