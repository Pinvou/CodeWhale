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

type f14ChargebackFailureOwner struct {
	billingAuthorizationExecutionRepository
	apply func(context.Context, ChargebackFinancialRepairExecutionInput) (ChargebackFinancialRepairResult, error)
	input ChargebackFinancialRepairExecutionInput
	calls int
}

func (s *f14ChargebackFailureOwner) ApplyChargebackFinancialRepair(ctx context.Context, input ChargebackFinancialRepairExecutionInput) (ChargebackFinancialRepairResult, error) {
	s.calls++
	s.input = input
	return s.apply(ctx, input)
}
func f14ChargebackFailureAttempt(t *testing.T, ctx context.Context, plan ChargebackFinancialRepairPlan, input ChargebackFinancialRepairExecutionInput, owner *f14ChargebackFailureOwner) authorizationDomainAttempt {
	t.Helper()
	critical, financial := chargebackFinancialRepairProofProjection(plan)
	normalized := encodeEntitlementCorrectionProjection(map[string]any{"chargeback_case_id": plan.ChargebackCaseID.String(), "payment_evidence_id": plan.EvidenceID.String(), "expected_case_version": plan.CaseVersion, "reason_code": chargebackFinancialRepairReason})
	binding := ApprovalAuthorizationBinding{ApprovalRequestID: input.ApprovalRequestID, EventID: input.AuthorizationEventID, ApprovalEpoch: input.ApprovalEpoch, CommandType: chargebackFinancialRepairCommand, PolicyVersion: 1, ExecutorIdentity: billingApprovalExecutorIdentity}
	event := approvalencoding.AuthorizationEvent{ApprovalRequestID: binding.ApprovalRequestID, EventID: binding.EventID, ApprovalControlEpoch: binding.ApprovalEpoch, CommandType: binding.CommandType, PolicyVersion: 1, ExecutorIdentity: binding.ExecutorIdentity, TargetResourceType: "CHARGEBACK_CASE", TargetResourceID: plan.ChargebackCaseID.String(), NormalizedCommand: normalized, CriticalResourceVersions: critical, FinancialSnapshot: financial}
	command := ApprovedCommand{ApprovalRequestID: binding.ApprovalRequestID, CommandType: binding.CommandType, PolicyVersion: 1, ExecutorIdentity: binding.ExecutorIdentity, TargetResourceType: event.TargetResourceType, TargetResourceID: event.TargetResourceID, NormalizedCommand: normalized, CriticalResourceVersions: critical, FinancialSnapshot: financial, RequesterUserID: uuid.New(), ApproverUserID: uuid.New(), DecisionReason: "independent original chargeback review", ExecutionIdempotencyKey: input.ExecutionKey}
	owner.billingAuthorizationExecutionRepository = &authorizationExecutionRepositoryStub{activations: ApprovalCommandActivationSnapshot{SchemaVersion: 1, Activations: []ApprovalCommandActivation{{CommandType: chargebackFinancialRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1, State: "ACTIVE", Version: input.ActivationVersion, UpdatedAt: time.Now().UTC()}}}}
	attempt := (&billingAuthorizationDomain{repository: owner}).ExecuteAuthorizationAttempt(ctx, binding, event, command)
	if owner.calls != 1 || !reflect.DeepEqual(owner.input, input) {
		t.Fatalf("failure did not reach exact frozen chargeback owner: calls=%d", owner.calls)
	}
	return attempt
}
func f14AssertChargebackFailure(t *testing.T, attempt authorizationDomainAttempt, definite bool) {
	t.Helper()
	expected := ""
	if definite {
		expected = "DOMAIN_PRECONDITION_REJECTED"
	}
	if attempt.FailureIsDefinite != definite || attempt.StableFailureCode != expected || len(attempt.Result.DomainResult) != 0 || len(attempt.Result.LedgerReferences) != 0 {
		t.Fatalf("wrong chargeback failure classification: %+v", attempt)
	}
}
func TestF14ChargebackCASInfrastructureClassification(t *testing.T) {
	plan := ChargebackFinancialRepairPlan{PolicyVersion: 1, ChargebackCaseID: uuid.New(), EvidenceID: uuid.New(), CaseVersion: 1, WalletID: uuid.New(), WalletVersion: 1, PackageID: uuid.New(), ProviderChargebackID: "controlled-original-chargeback", BaseShortfallMicrocredits: 100, ComponentDigest: strings.Repeat("a", 64), FactsDigest: strings.Repeat("b", 64)}
	input := chargebackFrozenInput(plan, 2)
	for _, c := range []struct {
		stage, name string
		cause       error
	}{
		{"chargeback_case_cas", "serialization", &pgconn.PgError{Code: "40001", Message: "controlled serialization fault"}},
		{"chargeback_case_receivable", "deadlock", &pgconn.PgError{Code: "40P01", Message: "controlled deadlock fault"}},
		{"anomaly_cas", "storage", &pgconn.PgError{Code: "58000", Message: "controlled storage fault"}},
		{"chargeback_case_cas", "canceled", context.Canceled},
		{"anomaly_cas", "deadline", context.DeadlineExceeded},
		{"chargeback_case_receivable", "zero-rows", nil},
	} {
		t.Run(c.stage+"/"+c.name, func(t *testing.T) {
			produced := chargebackRepairConflict(c.stage, c.cause)
			if produced == nil || !strings.Contains(produced.Error(), c.stage) || errors.Is(produced, ErrConflict) != (c.cause == nil) {
				t.Fatalf("wrong owner CAS identity: %v", produced)
			}
			if c.cause != nil && !errors.Is(produced, c.cause) {
				t.Fatal("original context/storage error lost")
			}
			if original, ok := c.cause.(*pgconn.PgError); ok {
				var got *pgconn.PgError
				if !errors.As(produced, &got) || got != original || got.Code != original.Code {
					t.Fatal("typed PostgreSQL error lost")
				}
			}
			f14AssertChargebackFailure(t, classifyAuthorizationDomainFailure(produced), c.cause == nil)
			owner := &f14ChargebackFailureOwner{apply: func(context.Context, ChargebackFinancialRepairExecutionInput) (ChargebackFinancialRepairResult, error) {
				return ChargebackFinancialRepairResult{}, produced
			}}
			f14AssertChargebackFailure(t, f14ChargebackFailureAttempt(t, context.Background(), plan, input, owner), c.cause == nil)
		})
	}
}
