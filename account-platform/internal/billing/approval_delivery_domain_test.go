package billing

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type authorizationQueryStep struct {
	contains string
	argument any
	values   []any
	err      error
}

type authorizationQueryStub struct {
	t         *testing.T
	steps     []authorizationQueryStep
	arguments [][]any
}

func (s *authorizationQueryStub) QueryRow(_ context.Context, query string, arguments ...any) pgx.Row {
	s.t.Helper()
	if len(s.steps) == 0 {
		s.t.Fatalf("unexpected authorization result query: %s", query)
	}
	step := s.steps[0]
	s.steps = s.steps[1:]
	if !strings.Contains(query, step.contains) {
		s.t.Fatalf("query %q does not contain %q", query, step.contains)
	}
	s.arguments = append(s.arguments, append([]any(nil), arguments...))
	return authorizationRow{values: step.values, err: step.err}
}

type authorizationRow struct {
	values []any
	err    error
}

func (r authorizationRow) Scan(destinations ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(destinations) != len(r.values) {
		return errors.New("authorization row arity mismatch")
	}
	for index := range destinations {
		destination := reflect.ValueOf(destinations[index])
		if destination.Kind() != reflect.Pointer || destination.IsNil() {
			return errors.New("authorization row destination is invalid")
		}
		if r.values[index] == nil {
			destination.Elem().Set(reflect.Zero(destination.Elem().Type()))
			continue
		}
		value := reflect.ValueOf(r.values[index])
		if !value.Type().AssignableTo(destination.Elem().Type()) {
			return errors.New("authorization row type mismatch")
		}
		destination.Elem().Set(value)
	}
	return nil
}

func TestBillingAuthorizationDomainResultFirstQueriesAreClosed(t *testing.T) {
	ctx := context.Background()
	requestID := uuid.New()
	adjustmentID, walletID, journalID := uuid.New(), uuid.New(), uuid.New()
	requestKey, executionKey, reason := "request-key", "execution-key", "approved because safe"
	approver, requester := uuid.New(), uuid.New()
	now := time.Date(2026, 9, 16, 3, 0, 0, 0, time.UTC)

	for _, test := range []struct {
		name, command string
		steps         []authorizationQueryStep
		assert        func(*testing.T, authorizationDomainResult)
	}{
		{
			name: "wallet", command: "WALLET_ADJUSTMENT",
			steps: []authorizationQueryStep{
				{contains: "manual_adjustments", argument: requestID, values: []any{adjustmentID}},
				{contains: "WHERE id=$1", argument: adjustmentID, values: []any{adjustmentID, walletID, int64(7), "reason", requester, &approver, nil, "POSTED", int64(4), &requestKey, &requestID, &executionKey, &journalID, &reason, now, now, &now}},
			},
			assert: func(t *testing.T, result authorizationDomainResult) {
				var payload map[string]any
				if json.Unmarshal(result.DomainResult, &payload) != nil || payload["status"] != "POSTED" || payload["adjustment_id"] != adjustmentID.String() {
					t.Fatalf("wallet result=%s", result.DomainResult)
				}
				if !strings.Contains(string(result.LedgerReferences), journalID.String()) {
					t.Fatalf("wallet ledger=%s", result.LedgerReferences)
				}
			},
		},
		{
			name: "provider", command: "PAYMENT_PROVIDER_RESUME",
			steps: []authorizationQueryStep{{contains: "payment_provider_resume_executions", argument: requestID, values: []any{uuid.New(), "provider-a", "RESUMED"}}},
			assert: func(t *testing.T, result authorizationDomainResult) {
				if !strings.Contains(string(result.DomainResult), `"provider_id":"provider-a"`) || string(result.LedgerReferences) != "{}" {
					t.Fatalf("provider result=%s ledger=%s", result.DomainResult, result.LedgerReferences)
				}
			},
		},
		{
			name: "product", command: "PAYMENT_PRODUCT_RESUME",
			steps: []authorizationQueryStep{{contains: "payment_product_resume_executions", argument: requestID, values: []any{uuid.New(), "provider-a", "product-a", "RESUMED"}}},
			assert: func(t *testing.T, result authorizationDomainResult) {
				if !strings.Contains(string(result.DomainResult), `"product_id":"product-a"`) || string(result.LedgerReferences) != "{}" {
					t.Fatalf("product result=%s ledger=%s", result.DomainResult, result.LedgerReferences)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			database := &authorizationQueryStub{t: t, steps: append([]authorizationQueryStep(nil), test.steps...)}
			domain := &billingAuthorizationDomain{database: database}
			result, found, err := domain.LookupAuthorizationResult(ctx, ApprovalAuthorizationBinding{ApprovalRequestID: requestID, CommandType: test.command}, approvalencoding.AuthorizationEvent{})
			if err != nil || !found || len(database.steps) != 0 {
				t.Fatalf("lookup found=%v remaining=%d err=%v", found, len(database.steps), err)
			}
			for index, arguments := range database.arguments {
				if len(arguments) != 1 || arguments[0] != test.steps[index].argument {
					t.Fatalf("query escaped request binding: %#v", arguments)
				}
			}
			test.assert(t, result)
		})
	}
}

func TestBillingAuthorizationDomainAbsenceAndErrorsRemainUnknown(t *testing.T) {
	for _, queryErr := range []error{pgx.ErrNoRows, errors.New("query unavailable")} {
		database := &authorizationQueryStub{t: t, steps: []authorizationQueryStep{{contains: "payment_provider_resume_executions", err: queryErr}}}
		domain := &billingAuthorizationDomain{database: database}
		_, found, err := domain.LookupAuthorizationResult(context.Background(), ApprovalAuthorizationBinding{ApprovalRequestID: uuid.New(), CommandType: "PAYMENT_PROVIDER_RESUME"}, approvalencoding.AuthorizationEvent{})
		if errors.Is(queryErr, pgx.ErrNoRows) {
			if err != nil || found {
				t.Fatalf("absence became result/error: found=%v err=%v", found, err)
			}
		} else if err == nil || found {
			t.Fatalf("query error became absence: found=%v err=%v", found, err)
		}
	}
}

type authorizationExecutionRepositoryStub struct {
	adjustment                      ManualAdjustment
	adjustmentErr, adjustmentRunErr error
	provider                        ProviderResumeVersions
	providerErr, providerRunErr     error
	product                         ProductResumeVersions
	productErr, productRunErr       error
	entitlementCorrection           EntitlementCorrectionResult
	entitlementCorrectionErr        error
	entitlementRunErr               error
	entitlementFrozen               entitlementCorrectionFrozenFacts
	entitlementKey                  string
	entitlementReasonCode           string
	ledgerRepair                    LedgerRepairResult
	ledgerRepairErr                 error
	ledgerRunErr                    error
	ledgerInput                     LedgerRepairExecutionInput
	writeOff                        ReceivableWriteOffResult
	writeOffErr                     error
	writeOffRunErr                  error
	writeOffInput                   ReceivableWriteOffExecutionInput
	activations                     ApprovalCommandActivationSnapshot
	activationsErr                  error
	adjustmentRuns                  int
	providerRuns, productRuns       int
	entitlementRuns                 int
	ledgerRuns                      int
	writeOffRuns                    int
}

func (s *authorizationExecutionRepositoryStub) AdjustmentForExecution(context.Context, uuid.UUID) (ManualAdjustment, error) {
	return s.adjustment, s.adjustmentErr
}
func (s *authorizationExecutionRepositoryStub) ExecuteApprovedAdjustment(context.Context, uuid.UUID, uuid.UUID, string, uuid.UUID, string) (ManualAdjustment, error) {
	s.adjustmentRuns++
	posted := s.adjustment
	posted.Status = "POSTED"
	return posted, s.adjustmentRunErr
}
func (s *authorizationExecutionRepositoryStub) providerResumeVersions(context.Context, ProviderResumeVersions) (ProviderResumeVersions, error) {
	return s.provider, s.providerErr
}
func (s *authorizationExecutionRepositoryStub) productResumeVersions(context.Context, ProductResumeVersions) (ProductResumeVersions, error) {
	return s.product, s.productErr
}
func (s *authorizationExecutionRepositoryStub) executeProviderResume(context.Context, ProviderResumeVersions, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, string, PaymentOrder, ProviderOrder) (ProviderResumeResult, error) {
	s.providerRuns++
	return ProviderResumeResult{ExecutionID: uuid.New(), ProviderID: s.provider.ProviderID, Status: "RESUMED"}, s.providerRunErr
}
func (s *authorizationExecutionRepositoryStub) executeProductResume(context.Context, ProductResumeVersions, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, string, PaymentOrder, ProviderOrder) (ProductResumeResult, error) {
	s.productRuns++
	return ProductResumeResult{ExecutionID: uuid.New(), ProviderID: s.product.ProviderID, ProductID: s.product.ProductID, Status: "RESUMED"}, s.productRunErr
}

func (s *authorizationExecutionRepositoryStub) EntitlementCorrectionForRequest(context.Context, uuid.UUID) (EntitlementCorrectionResult, error) {
	return s.entitlementCorrection, s.entitlementCorrectionErr
}

func (s *authorizationExecutionRepositoryStub) ExecuteEntitlementComponentCorrection(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ int64, _, _, _, _ uuid.UUID, executionKey, _, reasonCode string, frozen entitlementCorrectionFrozenFacts) (EntitlementCorrectionResult, error) {
	s.entitlementRuns++
	s.entitlementFrozen = frozen
	s.entitlementKey = executionKey
	s.entitlementReasonCode = reasonCode
	if s.entitlementRunErr != nil {
		return EntitlementCorrectionResult{}, s.entitlementRunErr
	}
	return s.entitlementCorrection, nil
}

func (s *authorizationExecutionRepositoryStub) ApprovalCommandActivations(context.Context) (ApprovalCommandActivationSnapshot, error) {
	return s.activations, s.activationsErr
}

func (s *authorizationExecutionRepositoryStub) LedgerRepairForRequest(context.Context, uuid.UUID) (LedgerRepairResult, error) {
	return s.ledgerRepair, s.ledgerRepairErr
}

func (s *authorizationExecutionRepositoryStub) ApplyLedgerRepair(_ context.Context, input LedgerRepairExecutionInput) (LedgerRepairResult, error) {
	s.ledgerRuns++
	s.ledgerInput = input
	if s.ledgerRunErr != nil {
		return LedgerRepairResult{}, s.ledgerRunErr
	}
	return s.ledgerRepair, nil
}

func (s *authorizationExecutionRepositoryStub) ReceivableWriteOffForRequest(context.Context, uuid.UUID) (ReceivableWriteOffResult, error) {
	return s.writeOff, s.writeOffErr
}

func (s *authorizationExecutionRepositoryStub) ApplyReceivableWriteOff(_ context.Context, input ReceivableWriteOffExecutionInput) (ReceivableWriteOffResult, error) {
	s.writeOffRuns++
	s.writeOffInput = input
	if s.writeOffRunErr != nil {
		return ReceivableWriteOffResult{}, s.writeOffRunErr
	}
	return s.writeOff, nil
}

func TestBillingAuthorizationDomainWalletAttemptClassification(t *testing.T) {
	requester, approver := uuid.New(), uuid.New()
	adjustment := ManualAdjustment{ID: uuid.New(), WalletID: uuid.New(), AmountMicrocredits: 7, RequestedBy: requester, Version: 3}
	normalized, _ := json.Marshal(map[string]any{"adjustment_id": adjustment.ID, "wallet_id": adjustment.WalletID, "amount_microcredits": adjustment.AmountMicrocredits, "requested_by": requester})
	event := approvalencoding.AuthorizationEvent{CommandType: "WALLET_ADJUSTMENT", TargetResourceType: "MANUAL_ADJUSTMENT", TargetResourceID: adjustment.ID.String(), CriticalResourceVersions: json.RawMessage(`{"adjustment_version":3}`)}
	command := ApprovedCommand{CommandType: event.CommandType, TargetResourceType: event.TargetResourceType, TargetResourceID: event.TargetResourceID, NormalizedCommand: normalized, CriticalResourceVersions: event.CriticalResourceVersions, RequesterUserID: requester, ApproverUserID: approver, DecisionReason: "approved independently", ExecutionIdempotencyKey: "approval:stable"}

	repository := &authorizationExecutionRepositoryStub{adjustment: adjustment}
	domain := &billingAuthorizationDomain{repository: repository}
	binding := ApprovalAuthorizationBinding{ApprovalRequestID: uuid.New(), CommandType: event.CommandType}
	attempt := domain.ExecuteAuthorizationAttempt(context.Background(), binding, event, command)
	if len(attempt.Result.DomainResult) == 0 || attempt.FailureIsDefinite || repository.adjustmentRuns != 1 {
		t.Fatalf("wallet success attempt=%+v runs=%d", attempt, repository.adjustmentRuns)
	}
	repository.adjustmentErr = ErrNotFound
	attempt = domain.ExecuteAuthorizationAttempt(context.Background(), binding, event, command)
	if !attempt.FailureIsDefinite || attempt.StableFailureCode != "DOMAIN_RESOURCE_NOT_FOUND" || repository.adjustmentRuns != 1 {
		t.Fatalf("wallet missing classification=%+v runs=%d", attempt, repository.adjustmentRuns)
	}
	repository.adjustmentErr = nil
	repository.adjustmentRunErr = ErrConflict
	attempt = domain.ExecuteAuthorizationAttempt(context.Background(), binding, event, command)
	if !attempt.FailureIsDefinite || attempt.StableFailureCode != "DOMAIN_PRECONDITION_REJECTED" || repository.adjustmentRuns != 2 {
		t.Fatalf("wallet rolled-back conflict classification=%+v runs=%d", attempt, repository.adjustmentRuns)
	}
	repository.adjustmentRunErr = errors.New("commit outcome unknown")
	attempt = domain.ExecuteAuthorizationAttempt(context.Background(), binding, event, command)
	if attempt.FailureIsDefinite || len(attempt.Result.DomainResult) != 0 || repository.adjustmentRuns != 3 {
		t.Fatalf("wallet unknown outcome=%+v runs=%d", attempt, repository.adjustmentRuns)
	}
}

func TestBillingProofEraExecutionRejectsRevisionBeforeDomainEffect(t *testing.T) {
	adjustment := ManualAdjustment{ID: uuid.New(), WalletID: uuid.New(), AmountMicrocredits: 7, RequestedBy: uuid.New(), Version: 3}
	normalized, _ := json.Marshal(map[string]any{"adjustment_id": adjustment.ID, "wallet_id": adjustment.WalletID, "amount_microcredits": adjustment.AmountMicrocredits, "requested_by": adjustment.RequestedBy})
	critical := json.RawMessage(`{"adjustment_version":3}`)
	financial, _ := json.Marshal(map[string]any{"wallet_id": adjustment.WalletID, "delta_microcredits": adjustment.AmountMicrocredits})
	artifacts, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{
		CommandType: "WALLET_ADJUSTMENT", PolicyVersion: 1, SnapshotSchemaVersion: 1,
		TargetResourceType: "MANUAL_ADJUSTMENT", TargetResourceID: adjustment.ID.String(),
		NormalizedCommand: normalized, CriticalResourceVersions: critical, FinancialSnapshot: financial,
	})
	if err != nil {
		t.Fatal(err)
	}
	event := approvalencoding.AuthorizationEvent{CommandType: "WALLET_ADJUSTMENT", PolicyVersion: 1, TargetResourceType: "MANUAL_ADJUSTMENT", TargetResourceID: adjustment.ID.String(), NormalizedCommand: normalized, CriticalResourceVersions: critical, FinancialSnapshot: financial,
		ProofSchemaVersion: "1", CommandDigest: artifacts.CommandDigest, SnapshotDigest: artifacts.SnapshotDigest, DomainRevision: "1:" + strings.Repeat("a", 64), ApprovalControlEpoch: 2}
	command := ApprovedCommand{CommandType: event.CommandType, TargetResourceType: event.TargetResourceType, TargetResourceID: event.TargetResourceID,
		NormalizedCommand: normalized, CriticalResourceVersions: critical, FinancialSnapshot: financial,
		RequesterUserID: adjustment.RequestedBy, ApproverUserID: uuid.New(), DecisionReason: "approved independently", ExecutionIdempotencyKey: "approval:stable"}
	binding := ApprovalAuthorizationBinding{ApprovalRequestID: uuid.New(), CommandType: event.CommandType, ApprovalEpoch: 2, SerializationVersion: approvalencoding.ApprovalSerializationVersion}
	repository := &authorizationExecutionRepositoryStub{adjustment: adjustment}
	domain := &billingAuthorizationDomain{repository: repository, proof: func(_ context.Context, request approvalencoding.ProofRequest) (approvalencoding.ProofResponse, error) {
		return approvalencoding.ProofResponse{Verdict: "ALLOW", DomainRevision: "1:" + strings.Repeat("b", 64), CommandDigest: request.Artifacts.CommandDigest, SnapshotDigest: request.Artifacts.SnapshotDigest}, nil
	}}
	domain.priorClaim = func(context.Context, ApprovalAuthorizationBinding) (bool, error) { return false, nil }
	attempt := domain.ExecuteAuthorizationAttempt(context.Background(), binding, event, command)
	if !attempt.FailureIsDefinite || attempt.StableFailureCode != "DOMAIN_REVISION_CHANGED" || repository.adjustmentRuns != 0 {
		t.Fatalf("drift must not reach adjustment executor: %+v runs=%d", attempt, repository.adjustmentRuns)
	}
	domain.proof = func(_ context.Context, request approvalencoding.ProofRequest) (approvalencoding.ProofResponse, error) {
		return approvalencoding.ProofResponse{Verdict: "ALLOW", DomainRevision: event.DomainRevision, CommandDigest: request.Artifacts.CommandDigest, SnapshotDigest: request.Artifacts.SnapshotDigest}, nil
	}
	claims := 0
	domain.claim = func(context.Context, ApprovalAuthorizationBinding, approvalencoding.AuthorizationEvent) error {
		claims++
		return nil
	}
	attempt = domain.ExecuteAuthorizationAttempt(context.Background(), binding, event, command)
	if len(attempt.Result.DomainResult) == 0 || repository.adjustmentRuns != 1 || claims != 1 {
		t.Fatalf("matching owner proof should claim before local CAS: %+v runs=%d claims=%d", attempt, repository.adjustmentRuns, claims)
	}
	domain.claim = func(context.Context, ApprovalAuthorizationBinding, approvalencoding.AuthorizationEvent) error {
		return ErrApprovalSnapshotMismatch
	}
	attempt = domain.ExecuteAuthorizationAttempt(context.Background(), binding, event, command)
	if !attempt.FailureIsDefinite || attempt.StableFailureCode != "DOMAIN_REVISION_CHANGED" || repository.adjustmentRuns != 1 {
		t.Fatalf("claim-time drift reached wallet effect: %+v runs=%d", attempt, repository.adjustmentRuns)
	}
	domain.claim = func(context.Context, ApprovalAuthorizationBinding, approvalencoding.AuthorizationEvent) error {
		return errors.New("claim outcome unknown")
	}
	attempt = domain.ExecuteAuthorizationAttempt(context.Background(), binding, event, command)
	if attempt.FailureIsDefinite || repository.adjustmentRuns != 1 {
		t.Fatalf("uncertain claim outcome reached wallet effect: %+v runs=%d", attempt, repository.adjustmentRuns)
	}
	domain.priorClaim = func(context.Context, ApprovalAuthorizationBinding) (bool, error) { return true, nil }
	attempt = domain.ExecuteAuthorizationAttempt(context.Background(), binding, event, command)
	if attempt.FailureIsDefinite || repository.adjustmentRuns != 1 {
		t.Fatalf("existing claim must remain unknown without replay: %+v runs=%d", attempt, repository.adjustmentRuns)
	}
	domain.priorClaim = func(context.Context, ApprovalAuthorizationBinding) (bool, error) { return false, nil }
	provider := ProviderResumeVersions{ProviderID: "provider-a", ProviderVersion: "v1", ProviderControlVersion: 3, ProbeOrderID: uuid.New(), PolicyVersion: "p1", ActiveMappingSetFingerprint: strings.Repeat("a", 32)}
	providerRaw, _ := json.Marshal(provider)
	providerFinancial := json.RawMessage(`{"new_orders_enabled":false,"provider_control_version":3,"product_control_version":null}`)
	providerArtifacts, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{
		CommandType: "PAYMENT_PROVIDER_RESUME", PolicyVersion: 2, SnapshotSchemaVersion: 1,
		TargetResourceType: "PAYMENT_PROVIDER_CONTROL", TargetResourceID: provider.ProviderID,
		NormalizedCommand: providerRaw, CriticalResourceVersions: providerRaw, FinancialSnapshot: providerFinancial,
	})
	if err != nil {
		t.Fatal(err)
	}
	providerEvent := approvalencoding.AuthorizationEvent{CommandType: "PAYMENT_PROVIDER_RESUME", PolicyVersion: 2, TargetResourceType: "PAYMENT_PROVIDER_CONTROL", TargetResourceID: provider.ProviderID,
		NormalizedCommand: providerRaw, CriticalResourceVersions: providerRaw, FinancialSnapshot: providerFinancial,
		ProofSchemaVersion: "1", CommandDigest: providerArtifacts.CommandDigest, SnapshotDigest: providerArtifacts.SnapshotDigest, DomainRevision: "1:" + strings.Repeat("c", 64), ApprovalControlEpoch: 2}
	providerCommand := ApprovedCommand{CommandType: providerEvent.CommandType, TargetResourceType: providerEvent.TargetResourceType, TargetResourceID: providerEvent.TargetResourceID,
		NormalizedCommand: providerRaw, CriticalResourceVersions: providerRaw, FinancialSnapshot: providerFinancial,
		RequesterUserID: adjustment.RequestedBy, ApproverUserID: uuid.New(), DecisionReason: "approved independently"}
	probeCalls := 0
	domain.probe = func(context.Context, uuid.UUID) (PaymentOrder, ProviderOrder, uuid.UUID, error) {
		probeCalls++
		return PaymentOrder{}, ProviderOrder{}, uuid.Nil, errors.New("probe must not run on proof drift")
	}
	providerBinding := ApprovalAuthorizationBinding{ApprovalRequestID: uuid.New(), CommandType: providerEvent.CommandType, ApprovalEpoch: 2, SerializationVersion: approvalencoding.ApprovalSerializationVersion}
	attempt = domain.ExecuteAuthorizationAttempt(context.Background(), providerBinding, providerEvent, providerCommand)
	if !attempt.FailureIsDefinite || attempt.StableFailureCode != "DOMAIN_REVISION_CHANGED" || probeCalls != 0 {
		t.Fatalf("provider drift reached probe: %+v probes=%d", attempt, probeCalls)
	}
	domain.proof = func(_ context.Context, request approvalencoding.ProofRequest) (approvalencoding.ProofResponse, error) {
		return approvalencoding.ProofResponse{Verdict: "ALLOW", DomainRevision: providerEvent.DomainRevision, CommandDigest: request.Artifacts.CommandDigest, SnapshotDigest: request.Artifacts.SnapshotDigest}, nil
	}
	domain.claim = func(context.Context, ApprovalAuthorizationBinding, approvalencoding.AuthorizationEvent) error {
		return ErrApprovalSnapshotMismatch
	}
	attempt = domain.ExecuteAuthorizationAttempt(context.Background(), providerBinding, providerEvent, providerCommand)
	if !attempt.FailureIsDefinite || attempt.StableFailureCode != "DOMAIN_REVISION_CHANGED" || probeCalls != 0 {
		t.Fatalf("provider claim drift reached probe: %+v probes=%d", attempt, probeCalls)
	}
	domain.claim = func(context.Context, ApprovalAuthorizationBinding, approvalencoding.AuthorizationEvent) error {
		return errors.New("unknown commit")
	}
	attempt = domain.ExecuteAuthorizationAttempt(context.Background(), providerBinding, providerEvent, providerCommand)
	if attempt.FailureIsDefinite || probeCalls != 0 {
		t.Fatalf("unknown provider claim reached probe: %+v probes=%d", attempt, probeCalls)
	}
}

func TestBillingAuthorizationDomainProviderAndProductProbeBoundary(t *testing.T) {
	requester, approver := uuid.New(), uuid.New()
	provider := ProviderResumeVersions{ProviderID: "provider-a", ProviderVersion: "v1", ProviderControlVersion: 3, ProbeOrderID: uuid.New(), PolicyVersion: "p1", ActiveMappingSetFingerprint: strings.Repeat("a", 64)}
	product := ProductResumeVersions{ProviderID: "provider-a", ProviderVersion: "v1", ProductID: "product-a", ProviderControlVersion: 3, ProductControlVersion: 4, CatalogItemID: uuid.New(), CatalogVersion: 5, AdmissionVersion: 6, MappingID: uuid.New(), MappingVersion: 7, ProbeOrderID: provider.ProbeOrderID, PolicyVersion: "p1"}

	for _, test := range []struct {
		name, command, targetType, targetID string
		versions                            any
	}{
		{name: "provider", command: "PAYMENT_PROVIDER_RESUME", targetType: "PAYMENT_PROVIDER_CONTROL", targetID: provider.ProviderID, versions: provider},
		{name: "product", command: "PAYMENT_PRODUCT_RESUME", targetType: "PAYMENT_PRODUCT_CONTROL", targetID: product.ProductID, versions: product},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, _ := json.Marshal(test.versions)
			event := approvalencoding.AuthorizationEvent{CommandType: test.command, TargetResourceType: test.targetType, TargetResourceID: test.targetID, CriticalResourceVersions: raw}
			command := ApprovedCommand{CommandType: test.command, TargetResourceType: test.targetType, TargetResourceID: test.targetID, NormalizedCommand: raw, RequesterUserID: requester, ApproverUserID: approver, DecisionReason: "approved independently"}
			repository := &authorizationExecutionRepositoryStub{provider: provider, product: product}
			probeCalls := 0
			domain := &billingAuthorizationDomain{repository: repository, probe: func(context.Context, uuid.UUID) (PaymentOrder, ProviderOrder, uuid.UUID, error) {
				probeCalls++
				return PaymentOrder{ProductID: product.ProductID}, ProviderOrder{}, uuid.New(), errors.New("probe outcome unknown")
			}}
			binding := ApprovalAuthorizationBinding{ApprovalRequestID: uuid.New(), CommandType: event.CommandType}
			attempt := domain.ExecuteAuthorizationAttempt(context.Background(), binding, event, command)
			if attempt.FailureIsDefinite || len(attempt.Result.DomainResult) != 0 || probeCalls != 1 || repository.providerRuns+repository.productRuns != 0 {
				t.Fatalf("probe error crossed unknown boundary: attempt=%+v probes=%d provider=%d product=%d", attempt, probeCalls, repository.providerRuns, repository.productRuns)
			}
			if test.command == "PAYMENT_PROVIDER_RESUME" {
				repository.provider.ProviderControlVersion++
			} else {
				repository.product.ProductControlVersion++
			}
			attempt = domain.ExecuteAuthorizationAttempt(context.Background(), binding, event, command)
			if !attempt.FailureIsDefinite || attempt.StableFailureCode != "COMMAND_SNAPSHOT_MISMATCH" || probeCalls != 1 {
				t.Fatalf("pre-probe drift was not a definite zero-probe failure: attempt=%+v probes=%d", attempt, probeCalls)
			}

			// Restore the authoritative snapshot and prove that the adapter dispatches
			// exactly one production-domain call after one successful trusted probe.
			repository.provider, repository.product = provider, product
			domain.probe = func(context.Context, uuid.UUID) (PaymentOrder, ProviderOrder, uuid.UUID, error) {
				probeCalls++
				return PaymentOrder{ID: provider.ProbeOrderID, ProviderID: provider.ProviderID, ProviderVersion: provider.ProviderVersion, ProductID: product.ProductID}, ProviderOrder{ProviderStatus: "FAILED", ProviderTraceID: "trace"}, uuid.New(), nil
			}
			attempt = domain.ExecuteAuthorizationAttempt(context.Background(), binding, event, command)
			if attempt.FailureIsDefinite || len(attempt.Result.DomainResult) == 0 || probeCalls != 2 {
				t.Fatalf("successful adapter dispatch failed: attempt=%+v probes=%d", attempt, probeCalls)
			}
			if test.command == "PAYMENT_PROVIDER_RESUME" {
				if repository.providerRuns != 1 || repository.productRuns != 0 {
					t.Fatalf("provider adapter dispatches provider=%d product=%d", repository.providerRuns, repository.productRuns)
				}
				repository.providerRunErr = ErrConflict
			} else if repository.providerRuns != 0 || repository.productRuns != 1 {
				t.Fatalf("product adapter dispatches provider=%d product=%d", repository.providerRuns, repository.productRuns)
			} else {
				repository.productRunErr = ErrConflict
			}
			attempt = domain.ExecuteAuthorizationAttempt(context.Background(), binding, event, command)
			if attempt.FailureIsDefinite || len(attempt.Result.DomainResult) != 0 || probeCalls != 3 {
				t.Fatalf("post-probe conflict was not kept unknown: attempt=%+v probes=%d", attempt, probeCalls)
			}
			if test.command == "PAYMENT_PROVIDER_RESUME" {
				if repository.providerRuns != 2 || repository.productRuns != 0 {
					t.Fatalf("provider unknown dispatches provider=%d product=%d", repository.providerRuns, repository.productRuns)
				}
			} else if repository.providerRuns != 0 || repository.productRuns != 2 {
				t.Fatalf("product unknown dispatches provider=%d product=%d", repository.providerRuns, repository.productRuns)
			}
		})
	}
}
