package billing

import (
	"context"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"testing"
	"time"
)

// Narrow controlled declarations for retained Ledger assertions. Other methods
// are never called; embedding does not implement any later financial command.
type authorizationExecutionRepositoryStub struct {
	billingAuthorizationExecutionRepository
	activations     ApprovalCommandActivationSnapshot
	ledgerRepair    LedgerRepairResult
	ledgerRepairErr error
	ledgerRunErr    error
	ledgerInput     LedgerRepairExecutionInput
	ledgerRuns      int
}

func (s *authorizationExecutionRepositoryStub) ApprovalCommandActivations(context.Context) (ApprovalCommandActivationSnapshot, error) {
	return s.activations, nil
}
func (s *authorizationExecutionRepositoryStub) LedgerRepairForRequest(context.Context, uuid.UUID) (LedgerRepairResult, error) {
	return s.ledgerRepair, s.ledgerRepairErr
}
func (s *authorizationExecutionRepositoryStub) ApplyLedgerRepair(_ context.Context, input LedgerRepairExecutionInput) (LedgerRepairResult, error) {
	s.ledgerRuns++
	s.ledgerInput = input
	return s.ledgerRepair, s.ledgerRunErr
}

type authorizationQueryStub struct{ t *testing.T }

func (s *authorizationQueryStub) QueryRow(context.Context, string, ...any) pgx.Row {
	s.t.Fatal("result must be queried from installed immutable Ledger owner")
	return nil
}

type billingProofFactsStub struct {
	billingProofFacts
	ledgerRepair    LedgerRepairPlan
	ledgerRepairErr error
}

func (s *billingProofFactsStub) ledgerRepairPlan(context.Context, LedgerRepairCommandInput) (LedgerRepairPlan, error) {
	return s.ledgerRepair, s.ledgerRepairErr
}

type billingApprovalProofStub struct {
	Repository
	*billingProofFactsStub
	activation ApprovalCommandActivationSnapshot
}

func (s *billingApprovalProofStub) ApprovalCommandActivations(context.Context) (ApprovalCommandActivationSnapshot, error) {
	return s.activation, nil
}
func validBillingApprovalActivationSnapshot(now time.Time) ApprovalCommandActivationSnapshot {
	result := ApprovalCommandActivationSnapshot{SchemaVersion: 1}
	for _, name := range billingApprovalActivationOrder {
		spec := billingApprovalActivationSpecs[name]
		result.Activations = append(result.Activations, ApprovalCommandActivation{CommandType: name, PolicyVersion: spec.policyVersion, SnapshotSchemaVersion: spec.snapshotSchemaVersion, State: "INACTIVE", Version: 1, UpdatedAt: now})
	}
	return result
}

// Retained integration tests use the current public wrapper and strict UUID
// fixture instead of an omitted old pagination fixture. No private test DDL.
func newAdminPaginationIntegrationRepository(t *testing.T, ctx context.Context, raw string) (*PostgresRepository, string) {
	return entitlementRepositoryToVersion(t, ctx, raw, 0)
}
