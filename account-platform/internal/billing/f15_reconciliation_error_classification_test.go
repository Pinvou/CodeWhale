package billing

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

type f15ReconciliationFailureOwner struct {
	billingAuthorizationExecutionRepository
	apply func(context.Context, ReconciliationFinancialRepairExecutionInput) (ReconciliationFinancialRepairResult, error)
	input ReconciliationFinancialRepairExecutionInput
	calls int
}

func (s *f15ReconciliationFailureOwner) ApplyReconciliationFinancialRepair(ctx context.Context, input ReconciliationFinancialRepairExecutionInput) (ReconciliationFinancialRepairResult, error) {
	s.calls++
	s.input = input
	return s.apply(ctx, input)
}
func f15ReconciliationFailureAttempt(t *testing.T, ctx context.Context, plan ReconciliationFinancialRepairPlan, input ReconciliationFinancialRepairExecutionInput, owner *f15ReconciliationFailureOwner) authorizationDomainAttempt {
	t.Helper()
	critical, financial := reconciliationFinancialRepairProofProjection(plan)
	normalized := encodeEntitlementCorrectionProjection(map[string]any{"reconciliation_case_id": plan.CaseID.String(), "payment_evidence_id": plan.PaymentEvidenceID.String(), "business_date": plan.BusinessDate, "resolution_code": reconciliationRepairResolutionCode})
	binding := ApprovalAuthorizationBinding{ApprovalRequestID: input.ApprovalRequestID, EventID: input.AuthorizationEventID, ApprovalEpoch: input.ApprovalEpoch, CommandType: reconciliationFinancialRepairCommand, PolicyVersion: 1, ExecutorIdentity: billingApprovalExecutorIdentity}
	event := approvalencoding.AuthorizationEvent{ApprovalRequestID: binding.ApprovalRequestID, EventID: binding.EventID, ApprovalControlEpoch: binding.ApprovalEpoch, CommandType: binding.CommandType, PolicyVersion: 1, ExecutorIdentity: binding.ExecutorIdentity, TargetResourceType: "RECONCILIATION_CASE", TargetResourceID: plan.CaseID.String(), NormalizedCommand: normalized, CriticalResourceVersions: critical, FinancialSnapshot: financial}
	command := ApprovedCommand{ApprovalRequestID: binding.ApprovalRequestID, CommandType: binding.CommandType, PolicyVersion: 1, ExecutorIdentity: binding.ExecutorIdentity, TargetResourceType: event.TargetResourceType, TargetResourceID: event.TargetResourceID, NormalizedCommand: normalized, CriticalResourceVersions: critical, FinancialSnapshot: financial, RequesterUserID: uuid.New(), ApproverUserID: uuid.New(), DecisionReason: "independent original reconciliation review", ExecutionIdempotencyKey: input.ExecutionKey}
	owner.billingAuthorizationExecutionRepository = &authorizationExecutionRepositoryStub{activations: ApprovalCommandActivationSnapshot{SchemaVersion: 1, Activations: []ApprovalCommandActivation{{CommandType: reconciliationFinancialRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1, State: "ACTIVE", Version: input.ActivationVersion, UpdatedAt: time.Now().UTC()}}}}
	attempt := (&billingAuthorizationDomain{repository: owner}).ExecuteAuthorizationAttempt(ctx, binding, event, command)
	if owner.calls != 1 || !reflect.DeepEqual(owner.input, input) {
		t.Fatalf("exact frozen reconciliation owner not reached: calls=%d definite=%t code=%s", owner.calls, attempt.FailureIsDefinite, attempt.StableFailureCode)
	}
	return attempt
}
func f15AssertReconciliationFailure(t *testing.T, attempt authorizationDomainAttempt, definite bool) {
	t.Helper()
	expected := ""
	if definite {
		expected = "DOMAIN_PRECONDITION_REJECTED"
	}
	if attempt.FailureIsDefinite != definite || attempt.StableFailureCode != expected || len(attempt.Result.DomainResult) != 0 || len(attempt.Result.LedgerReferences) != 0 {
		t.Fatalf("wrong reconciliation failure classification: %+v", attempt)
	}
}
func TestF15ReconciliationCASInfrastructureClassification(t *testing.T) {
	plan := ReconciliationFinancialRepairPlan{PolicyVersion: 1, CaseID: uuid.New(), RunID: uuid.New(), PaymentEvidenceID: uuid.New(), CaseVersion: 1, Channel: "ALIPAY", BusinessDate: "2026-10-13", MerchantOrderNo: "controlled-original-merchant", AmountFen: 100, Currency: "CNY", ProviderTransactionID: "controlled-original-provider", ProviderSnapshotDigest: strings.Repeat("a", 64), FactsDigest: strings.Repeat("b", 64)}
	input := reconciliationFrozenInput(plan, 2)
	for _, c := range []struct {
		stage, name string
		cause       error
	}{
		{"reconciliation_case_cas", "serialization", &pgconn.PgError{Code: "40001", Message: "controlled serialization fault"}},
		{"manual_review_cas", "deadlock", &pgconn.PgError{Code: "40P01", Message: "controlled deadlock fault"}},
		{"reconciliation_case_cas", "storage", &pgconn.PgError{Code: "58000", Message: "controlled storage fault"}},
		{"manual_review_cas", "canceled", context.Canceled},
		{"reconciliation_case_cas", "deadline", context.DeadlineExceeded},
		{"manual_review_cas", "zero-rows", nil},
	} {
		t.Run(c.stage+"/"+c.name, func(t *testing.T) {
			produced := reconciliationRepairConflict(c.stage, c.cause)
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
			f15AssertReconciliationFailure(t, classifyAuthorizationDomainFailure(produced), c.cause == nil)
			owner := &f15ReconciliationFailureOwner{apply: func(context.Context, ReconciliationFinancialRepairExecutionInput) (ReconciliationFinancialRepairResult, error) {
				return ReconciliationFinancialRepairResult{}, produced
			}}
			f15AssertReconciliationFailure(t, f15ReconciliationFailureAttempt(t, context.Background(), plan, input, owner), c.cause == nil)
		})
	}
}
