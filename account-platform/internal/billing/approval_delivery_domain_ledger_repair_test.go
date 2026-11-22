package billing

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
)

func ledgerRepairCommandFixture() (ApprovalAuthorizationBinding, approvalencoding.AuthorizationEvent, ApprovedCommand) {
	journalEntryID, targetPostingID := uuid.New(), uuid.New()
	wrongAccountID, correctAccountID := uuid.New(), uuid.New()
	requesterID, approverID := uuid.New(), uuid.New()
	normalized, _ := json.Marshal(map[string]any{
		"target_journal_entry_id": journalEntryID, "target_posting_id": targetPostingID,
		"wrong_ledger_account_id": wrongAccountID, "correct_ledger_account_id": correctAccountID,
		"amount_microcredits": 30, "reason_code": "ATTRIBUTION_TO_WRONG_PLATFORM_ACCOUNT",
	})
	critical, _ := json.Marshal(map[string]any{
		"target_journal_entry_id": journalEntryID, "target_posting_id": targetPostingID,
		"posting_set_digest": strings.Repeat("a", 64), "entry_type": "USAGE", "policy_version": 1,
	})
	financial, _ := json.Marshal(map[string]any{
		"amount_microcredits": 30, "wrong_ledger_account_id": wrongAccountID, "correct_ledger_account_id": correctAccountID,
		"wrong_account_type": "ADJUSTMENT", "correct_account_type": "USAGE_REVENUE", "wallet_id": nil,
	})
	binding := ApprovalAuthorizationBinding{
		ApprovalRequestID: uuid.New(), ApprovalEpoch: 5, PolicyVersion: 1, CommandType: ledgerRepairCommand,
		EventID: uuid.New(), ExecutorIdentity: billingApprovalExecutorIdentity,
	}
	event := approvalencoding.AuthorizationEvent{
		CommandType: ledgerRepairCommand, PolicyVersion: 1,
		EventID: binding.EventID, ApprovalRequestID: binding.ApprovalRequestID,
		ExecutorIdentity: billingApprovalExecutorIdentity, ApprovalControlEpoch: binding.ApprovalEpoch,
		TargetResourceType: "LEDGER_POSTING", TargetResourceID: targetPostingID.String(),
		NormalizedCommand: normalized, CriticalResourceVersions: critical, FinancialSnapshot: financial,
	}
	command := ApprovedCommand{
		CommandType: event.CommandType, PolicyVersion: 1, TargetResourceType: event.TargetResourceType,
		TargetResourceID: event.TargetResourceID, NormalizedCommand: normalized,
		CriticalResourceVersions: critical, FinancialSnapshot: financial,
		ApprovalRequestID: binding.ApprovalRequestID, ExecutorIdentity: billingApprovalExecutorIdentity,
		RequesterUserID: requesterID, ApproverUserID: approverID,
		DecisionReason: "independent review completed", ExecutionIdempotencyKey: "approval:ledger-repair:stable",
	}
	return binding, event, command
}

func activeLedgerRepairFixture(version int64) ApprovalCommandActivationSnapshot {
	return ApprovalCommandActivationSnapshot{SchemaVersion: 1, Activations: []ApprovalCommandActivation{{
		CommandType: ledgerRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1,
		State: "ACTIVE", Version: version, UpdatedAt: time.Now().UTC(),
	}}}
}

func TestBillingAuthorizationDomainLedgerRepairMapping(t *testing.T) {
	binding, event, command := ledgerRepairCommandFixture()
	repository := &authorizationExecutionRepositoryStub{
		activations:  activeLedgerRepairFixture(4),
		ledgerRepair: LedgerRepairResult{CaseID: uuid.New(), JournalEntryID: uuid.New(), Status: "SUCCEEDED"},
	}
	domain := &billingAuthorizationDomain{repository: repository}

	attempt := domain.ExecuteAuthorizationAttempt(context.Background(), binding, event, command)
	if attempt.FailureIsDefinite || len(attempt.Result.DomainResult) == 0 || repository.ledgerRuns != 1 {
		t.Fatalf("ledger repair attempt=%+v runs=%d", attempt, repository.ledgerRuns)
	}
	input := repository.ledgerInput
	if input.ApprovalRequestID != binding.ApprovalRequestID || input.AuthorizationEventID != binding.EventID ||
		input.ApprovalEpoch != binding.ApprovalEpoch || input.ExecutionKey != command.ExecutionIdempotencyKey ||
		input.PolicyVersion != 1 || input.SchemaVersion != 1 || input.ActivationVersion != 4 ||
		input.Command.TargetPostingID.String() != event.TargetResourceID || input.Command.AmountMicrocredits != 30 ||
		input.Frozen.EntryType != "USAGE" || input.Frozen.AmountMicrocredits != 30 ||
		input.Frozen.PostingSetDigest != strings.Repeat("a", 64) || input.Frozen.FactsDigest != "" {
		t.Fatalf("ledger repair input=%+v", input)
	}
	if !strings.Contains(string(attempt.Result.LedgerReferences), `"journal_entry_id"`) {
		t.Fatalf("ledger repair ledger reference=%s", attempt.Result.LedgerReferences)
	}

	// A tampered target never reaches the domain effect.
	tampered := event
	tampered.TargetResourceID = uuid.NewString()
	attempt = domain.ExecuteAuthorizationAttempt(context.Background(), binding, tampered, command)
	if !attempt.FailureIsDefinite || attempt.StableFailureCode != "COMMAND_SNAPSHOT_MISMATCH" || repository.ledgerRuns != 1 {
		t.Fatalf("tampered target attempt=%+v runs=%d", attempt, repository.ledgerRuns)
	}

	// An inactive or version-mismatched activation is a terminal frozen failure.
	repository.activations = ApprovalCommandActivationSnapshot{SchemaVersion: 1, Activations: []ApprovalCommandActivation{{
		CommandType: ledgerRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1,
		State: "INACTIVE", Version: 4, UpdatedAt: time.Now().UTC(),
	}}}
	attempt = domain.ExecuteAuthorizationAttempt(context.Background(), binding, event, command)
	if !attempt.FailureIsDefinite || attempt.StableFailureCode != "POLICY_OR_SCHEMA_MISMATCH" || repository.ledgerRuns != 1 {
		t.Fatalf("inactive activation attempt=%+v runs=%d", attempt, repository.ledgerRuns)
	}

	repository.activations = activeLedgerRepairFixture(4)
	repository.ledgerRunErr = ErrConflict
	attempt = domain.ExecuteAuthorizationAttempt(context.Background(), binding, event, command)
	if !attempt.FailureIsDefinite || attempt.StableFailureCode != "DOMAIN_PRECONDITION_REJECTED" || repository.ledgerRuns != 2 {
		t.Fatalf("ledger repair conflict classification=%+v runs=%d", attempt, repository.ledgerRuns)
	}
	repository.ledgerRunErr = errors.New("commit outcome unknown")
	attempt = domain.ExecuteAuthorizationAttempt(context.Background(), binding, event, command)
	if attempt.FailureIsDefinite || len(attempt.Result.DomainResult) != 0 || repository.ledgerRuns != 3 {
		t.Fatalf("ledger repair unknown outcome=%+v runs=%d", attempt, repository.ledgerRuns)
	}
}

func TestBillingAuthorizationDomainLedgerRepairResultFirstRead(t *testing.T) {
	binding, event, _ := ledgerRepairCommandFixture()
	repair := LedgerRepairResult{CaseID: uuid.New(), JournalEntryID: uuid.New(), Status: "SUCCEEDED"}
	domain := &billingAuthorizationDomain{database: &authorizationQueryStub{t: t}, repository: &authorizationExecutionRepositoryStub{ledgerRepair: repair}}
	result, found, err := domain.LookupAuthorizationResult(context.Background(), binding, event)
	if err != nil || !found || !strings.Contains(string(result.DomainResult), repair.CaseID.String()) ||
		!strings.Contains(string(result.LedgerReferences), repair.JournalEntryID.String()) {
		t.Fatalf("result-first repair=%+v found=%v err=%v", result, found, err)
	}

	domain = &billingAuthorizationDomain{database: &authorizationQueryStub{t: t}, repository: &authorizationExecutionRepositoryStub{ledgerRepairErr: ErrNotFound}}
	result, found, err = domain.LookupAuthorizationResult(context.Background(), binding, event)
	if err != nil || found || len(result.DomainResult) != 0 {
		t.Fatalf("absent repair result=%+v found=%v err=%v", result, found, err)
	}
	domain = &billingAuthorizationDomain{database: &authorizationQueryStub{t: t}, repository: &authorizationExecutionRepositoryStub{ledgerRepairErr: errors.New("read unavailable")}}
	if _, found, err = domain.LookupAuthorizationResult(context.Background(), binding, event); err == nil || found {
		t.Fatalf("read error became absence: found=%v err=%v", found, err)
	}
}

func TestBillingAuthorizationDomainLedgerRepairProofUsesSchemaV1BeforeEffect(t *testing.T) {
	binding, event, command := ledgerRepairCommandFixture()
	artifacts, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{
		CommandType: event.CommandType, PolicyVersion: 1, SnapshotSchemaVersion: approvalSnapshotSchemaVersion(ledgerRepairCommand),
		TargetResourceType: event.TargetResourceType, TargetResourceID: event.TargetResourceID,
		NormalizedCommand: event.NormalizedCommand, CriticalResourceVersions: event.CriticalResourceVersions,
		FinancialSnapshot: event.FinancialSnapshot,
	})
	if err != nil {
		t.Fatal(err)
	}
	event.ProofSchemaVersion = approvalencoding.ProofSchemaVersion
	event.CommandDigest = artifacts.CommandDigest
	event.SnapshotDigest = artifacts.SnapshotDigest
	event.DomainRevision = "4:" + strings.Repeat("c", 64)
	binding.SerializationVersion = approvalencoding.ApprovalSerializationVersion

	repository := &authorizationExecutionRepositoryStub{
		activations:  activeLedgerRepairFixture(4),
		ledgerRepair: LedgerRepairResult{CaseID: uuid.New(), JournalEntryID: uuid.New(), Status: "SUCCEEDED"},
	}
	proofCalls, claims := 0, 0
	domain := &billingAuthorizationDomain{
		repository: repository,
		priorClaim: func(context.Context, ApprovalAuthorizationBinding) (bool, error) { return false, nil },
		proof: func(_ context.Context, request approvalencoding.ProofRequest) (approvalencoding.ProofResponse, error) {
			proofCalls++
			if request.Artifacts.PolicyVersion != 1 || request.Artifacts.SnapshotSchemaVersion != 1 ||
				request.Artifacts.CommandDigest != event.CommandDigest || request.Artifacts.SnapshotDigest != event.SnapshotDigest {
				t.Fatalf("proof artifacts=%+v", request.Artifacts)
			}
			return approvalencoding.ProofResponse{Verdict: "ALLOW", DomainRevision: event.DomainRevision,
				CommandDigest: event.CommandDigest, SnapshotDigest: event.SnapshotDigest}, nil
		},
		claim: func(context.Context, ApprovalAuthorizationBinding, approvalencoding.AuthorizationEvent) error {
			claims++
			return nil
		},
	}
	attempt := domain.ExecuteAuthorizationAttempt(context.Background(), binding, event, command)
	if attempt.FailureIsDefinite || proofCalls != 1 || claims != 1 || repository.ledgerRuns != 1 {
		t.Fatalf("v1 proof path attempt=%+v proof=%d claims=%d runs=%d", attempt, proofCalls, claims, repository.ledgerRuns)
	}

	domain.proof = func(context.Context, approvalencoding.ProofRequest) (approvalencoding.ProofResponse, error) {
		proofCalls++
		return approvalencoding.ProofResponse{}, errors.New("proof unavailable")
	}
	attempt = domain.ExecuteAuthorizationAttempt(context.Background(), binding, event, command)
	if attempt.FailureIsDefinite || repository.ledgerRuns != 1 || proofCalls != 2 {
		t.Fatalf("proof failure crossed effect boundary: %+v runs=%d proof=%d", attempt, repository.ledgerRuns, proofCalls)
	}
}
