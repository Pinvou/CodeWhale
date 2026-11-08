package billing

import (
	"context"
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

type f16UsageFailureOwner struct {
	billingAuthorizationExecutionRepository
	apply func(context.Context, UsageSettlementRepairExecutionInput) (UsageSettlementRepairResult, error)
	input UsageSettlementRepairExecutionInput
	calls int
}

func (s *f16UsageFailureOwner) ApplyUsageSettlementRepair(ctx context.Context, input UsageSettlementRepairExecutionInput) (UsageSettlementRepairResult, error) {
	s.calls++
	s.input = input
	return s.apply(ctx, input)
}
func f16UsageFailureAttempt(t *testing.T, ctx context.Context, plan UsageSettlementRepairPlan, input UsageSettlementRepairExecutionInput, owner *f16UsageFailureOwner) authorizationDomainAttempt {
	t.Helper()
	critical, financial := usageSettlementRepairProofProjection(plan)
	normalized := encodeEntitlementCorrectionProjection(map[string]any{"usage_request_id": plan.UsageRequestID.String(), "hold_id": plan.HoldID.String(), "loss_review_id": plan.LossReviewID.String(), "evidence_kind": usageSettlementRepairEvidenceKind, "reason_code": usageSettlementRepairReason})
	binding := ApprovalAuthorizationBinding{ApprovalRequestID: input.ApprovalRequestID, EventID: input.AuthorizationEventID, ApprovalEpoch: input.ApprovalEpoch, CommandType: usageSettlementRepairCommand, PolicyVersion: 1, ExecutorIdentity: billingApprovalExecutorIdentity}
	event := approvalencoding.AuthorizationEvent{ApprovalRequestID: binding.ApprovalRequestID, EventID: binding.EventID, ApprovalControlEpoch: binding.ApprovalEpoch, CommandType: binding.CommandType, PolicyVersion: 1, ExecutorIdentity: binding.ExecutorIdentity, TargetResourceType: "USAGE_HOLD", TargetResourceID: plan.HoldID.String(), NormalizedCommand: normalized, CriticalResourceVersions: critical, FinancialSnapshot: financial}
	command := ApprovedCommand{ApprovalRequestID: binding.ApprovalRequestID, CommandType: binding.CommandType, PolicyVersion: 1, ExecutorIdentity: binding.ExecutorIdentity, TargetResourceType: event.TargetResourceType, TargetResourceID: event.TargetResourceID, NormalizedCommand: normalized, CriticalResourceVersions: critical, FinancialSnapshot: financial, RequesterUserID: uuid.New(), ApproverUserID: uuid.New(), DecisionReason: "independent original usage settlement review", ExecutionIdempotencyKey: input.ExecutionKey}
	owner.billingAuthorizationExecutionRepository = &authorizationExecutionRepositoryStub{activations: ApprovalCommandActivationSnapshot{SchemaVersion: 1, Activations: []ApprovalCommandActivation{{CommandType: usageSettlementRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1, State: "ACTIVE", Version: input.ActivationVersion, UpdatedAt: time.Now().UTC()}}}}
	attempt := (&billingAuthorizationDomain{repository: owner}).ExecuteAuthorizationAttempt(ctx, binding, event, command)
	if owner.calls != 1 || !reflect.DeepEqual(owner.input, input) {
		t.Fatalf("exact frozen usage owner not reached: calls=%d definite=%t code=%s", owner.calls, attempt.FailureIsDefinite, attempt.StableFailureCode)
	}
	return attempt
}
func f16AssertUsageFailure(t *testing.T, attempt authorizationDomainAttempt, definite bool) {
	t.Helper()
	expected := ""
	if definite {
		expected = "DOMAIN_PRECONDITION_REJECTED"
	}
	if attempt.FailureIsDefinite != definite || attempt.StableFailureCode != expected || len(attempt.Result.DomainResult) != 0 || len(attempt.Result.LedgerReferences) != 0 {
		t.Fatalf("wrong usage failure classification: %+v", attempt)
	}
}
func TestF16UsageCASInfrastructureClassification(t *testing.T) {
	now := time.Now().UTC()
	plan := UsageSettlementRepairPlan{PolicyVersion: 1, UsageRequestID: uuid.New(), HoldID: uuid.New(), LossReviewID: uuid.New(), HoldUpdatedAt: now, LossReviewUpdatedAt: now, WalletID: uuid.New(), WalletVersion: 1, ExpectedMicrocredits: 150, SettledMicrocredits: 100, OverageMicrocredits: 50, AllocationsDigest: strings.Repeat("a", 64), FactsDigest: strings.Repeat("b", 64)}
	input := UsageSettlementRepairExecutionInput{ApprovalRequestID: uuid.New(), AuthorizationEventID: uuid.New(), ApprovalEpoch: 1, ExecutionKey: "usage-repair-controlled-identity", PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: 2, Command: UsageSettlementRepairCommandInput{UsageRequestID: plan.UsageRequestID, HoldID: plan.HoldID, LossReviewID: plan.LossReviewID, EvidenceKind: usageSettlementRepairEvidenceKind}, Frozen: UsageSettlementRepairFrozenFacts{HoldUpdatedAt: now, LossReviewUpdatedAt: now, ExpectedMicrocredits: 150, SettledMicrocredits: 100, WalletID: plan.WalletID, WalletVersion: 1, AllocationsDigest: plan.AllocationsDigest, FactsDigest: plan.FactsDigest}}
	for _, c := range []struct {
		stage, name string
		cause       error
	}{
		{"allocation_backfill", "serialization", &pgconn.PgError{Code: "40001", Message: "controlled serialization fault"}},
		{"credit_lot_cas", "deadlock", &pgconn.PgError{Code: "40P01", Message: "controlled deadlock fault"}},
		{"component_cas", "storage", &pgconn.PgError{Code: "58000", Message: "controlled storage fault"}},
		{"wallet_cas", "canceled", context.Canceled},
		{"hold_cas", "deadline", context.DeadlineExceeded},
		{"loss_review_cas", "connection-closed", io.ErrUnexpectedEOF},
		{"hold_cas", "zero-rows", nil},
	} {
		t.Run(c.stage+"/"+c.name, func(t *testing.T) {
			produced := usageSettlementRepairConflict(c.stage, c.cause)
			if produced == nil || !strings.Contains(produced.Error(), c.stage) || errors.Is(produced, ErrConflict) != (c.cause == nil) {
				t.Fatalf("wrong owner CAS identity: %v", produced)
			}
			if c.cause != nil && !errors.Is(produced, c.cause) {
				t.Fatal("original context/storage cause lost")
			}
			if original, ok := c.cause.(*pgconn.PgError); ok {
				var got *pgconn.PgError
				if !errors.As(produced, &got) || got != original || got.Code != original.Code {
					t.Fatal("typed PostgreSQL error lost")
				}
			}
			f16AssertUsageFailure(t, classifyAuthorizationDomainFailure(produced), c.cause == nil)
			owner := &f16UsageFailureOwner{apply: func(context.Context, UsageSettlementRepairExecutionInput) (UsageSettlementRepairResult, error) {
				return UsageSettlementRepairResult{}, produced
			}}
			f16AssertUsageFailure(t, f16UsageFailureAttempt(t, context.Background(), plan, input, owner), c.cause == nil)
		})
	}
}
