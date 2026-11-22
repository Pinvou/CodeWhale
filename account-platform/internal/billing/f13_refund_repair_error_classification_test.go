package billing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// The adapter controls only the owner result. The real authorization mapping and
// classifier still execute, so an early rejection cannot satisfy these tests.
type f13RefundFailureOwner struct {
	billingAuthorizationExecutionRepository
	apply func(context.Context, RefundExceptionRepairExecutionInput) (RefundExceptionRepairResult, error)
	input RefundExceptionRepairExecutionInput
	calls int
}

func (s *f13RefundFailureOwner) ApplyRefundExceptionRepair(ctx context.Context, input RefundExceptionRepairExecutionInput) (RefundExceptionRepairResult, error) {
	s.calls++
	s.input = input
	return s.apply(ctx, input)
}
func f13RefundFailureAuthorization(t *testing.T, input RefundExceptionRepairExecutionInput) (ApprovalAuthorizationBinding, approvalencoding.AuthorizationEvent, ApprovedCommand) {
	t.Helper()
	marshal := func(v any) json.RawMessage {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	normalized := marshal(map[string]any{"refund_id": input.Command.RefundID, "payment_evidence_id": input.Command.PaymentEvidenceID, "anomaly_case_id": input.Command.AnomalyCaseID, "reason_code": refundExceptionRepairReason})
	critical := marshal(map[string]any{"refund_updated_at": input.Frozen.RefundUpdatedAt.UTC().Format(time.RFC3339Nano), "anomaly_version": input.Frozen.AnomalyVersion, "facts_digest": input.Frozen.FactsDigest, "policy_version": input.PolicyVersion})
	financial := marshal(map[string]any{"amount_fen": input.Frozen.AmountFen, "credit_microcredits": input.Frozen.CreditMicrocredits, "provider_refund_id": input.Frozen.ProviderRefundID, "revoked_microcredits": input.Frozen.RevokedMicrocredits, "shortfall_microcredits": input.Frozen.ShortfallMicrocredits, "wallet_id": input.Frozen.WalletID, "wallet_version": input.Frozen.WalletVersion, "package_id": input.Frozen.PackageID})
	binding := ApprovalAuthorizationBinding{ApprovalRequestID: input.ApprovalRequestID, EventID: input.AuthorizationEventID, ApprovalEpoch: input.ApprovalEpoch, CommandType: refundExceptionRepairCommand, PolicyVersion: 1, ExecutorIdentity: billingApprovalExecutorIdentity}
	event := approvalencoding.AuthorizationEvent{ApprovalRequestID: binding.ApprovalRequestID, EventID: binding.EventID, ApprovalControlEpoch: binding.ApprovalEpoch, CommandType: binding.CommandType, PolicyVersion: 1, ExecutorIdentity: binding.ExecutorIdentity, TargetResourceType: "REFUND", TargetResourceID: input.Command.RefundID.String(), NormalizedCommand: normalized, CriticalResourceVersions: critical, FinancialSnapshot: financial}
	command := ApprovedCommand{ApprovalRequestID: binding.ApprovalRequestID, CommandType: binding.CommandType, PolicyVersion: 1, ExecutorIdentity: binding.ExecutorIdentity, TargetResourceType: event.TargetResourceType, TargetResourceID: event.TargetResourceID, NormalizedCommand: normalized, CriticalResourceVersions: critical, FinancialSnapshot: financial, RequesterUserID: uuid.New(), ApproverUserID: uuid.New(), DecisionReason: "independent original refund repair review", ExecutionIdempotencyKey: input.ExecutionKey}
	return binding, event, command
}
func f13RefundFailureDomain(owner *f13RefundFailureOwner, activation int64) *billingAuthorizationDomain {
	owner.billingAuthorizationExecutionRepository = &authorizationExecutionRepositoryStub{activations: ApprovalCommandActivationSnapshot{SchemaVersion: 1, Activations: []ApprovalCommandActivation{{CommandType: refundExceptionRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1, State: "ACTIVE", Version: activation, UpdatedAt: time.Now().UTC()}}}}
	return &billingAuthorizationDomain{repository: owner}
}
func f13AssertRefundFailureAttempt(t *testing.T, attempt authorizationDomainAttempt, definite bool) {
	t.Helper()
	if attempt.FailureIsDefinite != definite || len(attempt.Result.DomainResult) != 0 || len(attempt.Result.LedgerReferences) != 0 {
		t.Fatalf("wrong refund failure classification: %+v", attempt)
	}
	expected := ""
	if definite {
		expected = "DOMAIN_PRECONDITION_REJECTED"
	}
	if attempt.StableFailureCode != expected {
		t.Fatalf("wrong stable failure code: %q", attempt.StableFailureCode)
	}
}

func TestF13RefundExceptionCASInfrastructureClassification(t *testing.T) {
	plan := RefundExceptionRepairPlan{RefundID: uuid.New(), EvidenceID: uuid.New(), AnomalyCaseID: uuid.New(), RefundUpdatedAt: time.Now().UTC(), AnomalyVersion: 1, AmountFen: 100, CreditMicrocredits: 100, ProviderRefundID: "controlled-original-refund", RevokedMicrocredits: 100, WalletID: uuid.New(), WalletVersion: 1, PackageID: uuid.New(), FactsDigest: strings.Repeat("a", 64)}
	input := refundFrozenInput(plan)
	input.ActivationVersion = 2
	binding, event, command := f13RefundFailureAuthorization(t, input)
	cases := []struct {
		stage, name string
		cause       error
	}{
		{"credit_lot_cas", "serialization", &pgconn.PgError{Code: "40001", Message: "controlled serialization fault"}},
		{"component_cas", "deadlock", &pgconn.PgError{Code: "40P01", Message: "controlled deadlock fault"}},
		{"grant_cas", "storage", &pgconn.PgError{Code: "58000", Message: "controlled storage fault"}},
		{"wallet_cas", "canceled", context.Canceled},
		{"package_cas", "deadline", context.DeadlineExceeded},
		{"anomaly_cas", "connection-closed", io.ErrUnexpectedEOF},
		{"credit_lot_cas", "zero-rows", nil},
	}
	for _, c := range cases {
		t.Run(c.stage+"/"+c.name, func(t *testing.T) {
			stage := c.stage
			produced := refundExceptionConflict(stage, c.cause)
			if produced == nil || !strings.Contains(produced.Error(), stage) {
				t.Fatal("CAS stage identity lost")
			}
			if errors.Is(produced, ErrConflict) != (c.cause == nil) {
				t.Fatalf("infra error acquired definite conflict identity: %v", produced)
			}
			if c.cause != nil && !errors.Is(produced, c.cause) {
				t.Fatal("original error identity lost")
			}
			if original, ok := c.cause.(*pgconn.PgError); ok {
				var got *pgconn.PgError
				if !errors.As(produced, &got) || got != original || got.Code != original.Code {
					t.Fatal("typed PostgreSQL error lost")
				}
			}
			f13AssertRefundFailureAttempt(t, classifyAuthorizationDomainFailure(produced), c.cause == nil)
			owner := &f13RefundFailureOwner{apply: func(context.Context, RefundExceptionRepairExecutionInput) (RefundExceptionRepairResult, error) {
				return RefundExceptionRepairResult{}, produced
			}}
			attempt := f13RefundFailureDomain(owner, input.ActivationVersion).ExecuteAuthorizationAttempt(context.Background(), binding, event, command)
			f13AssertRefundFailureAttempt(t, attempt, c.cause == nil)
			if owner.calls != 1 || !reflect.DeepEqual(owner.input, input) {
				t.Fatalf("failure never reached exact frozen owner: calls=%d definite=%t code=%s", owner.calls, attempt.FailureIsDefinite, attempt.StableFailureCode)
			}
		})
	}
}
