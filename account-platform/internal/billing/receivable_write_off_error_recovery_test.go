package billing

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

type f11WriteOffUpdateControl struct {
	t         *testing.T
	statement string
	arguments []any
	tag       pgconn.CommandTag
	err       error
	calls     int
}

func (control *f11WriteOffUpdateControl) Exec(_ context.Context, statement string, arguments ...any) (pgconn.CommandTag, error) {
	control.t.Helper()
	control.calls++
	if statement != control.statement || !reflect.DeepEqual(arguments, control.arguments) {
		control.t.Fatal("actual guarded update did not retain its SQL and argument tuple")
	}
	return control.tag, control.err
}

// The two statements are the same constants invoked by production Apply. The
// controlled executor injects storage outcomes; the real authorization domain
// consumes their typed errors. This is not a PostgreSQL financial-effects test.
func TestF11WriteOffGuardedUpdatesKeepStorageFailuresUnknown(t *testing.T) {
	binding, event, command, plan := writeOffWireFixture()
	now := time.Now().UTC()
	for _, update := range []struct {
		name, sql string
		arguments []any
	}{
		{"receivable", receivableWriteOffReceivableUpdateSQL, []any{plan.ReceivableID, plan.ReceivableVersion, plan.WriteOffMicrocredits, now}},
		{"wallet", receivableWriteOffWalletUpdateSQL, []any{plan.WalletID, plan.WriteOffMicrocredits, now}},
	} {
		for _, outcome := range []struct {
			name     string
			err      error
			rows     string
			definite bool
		}{
			{"serialization", fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "40001"}), "UPDATE 0", false},
			{"deadlock", &pgconn.PgError{Code: "40P01"}, "UPDATE 0", false},
			{"connection", &pgconn.PgError{Code: "08006"}, "UPDATE 0", false},
			{"canceled", context.Canceled, "UPDATE 0", false},
			{"zero-rows", nil, "UPDATE 0", true},
			{"one-row", nil, "UPDATE 1", false},
		} {
			t.Run(update.name+"/"+outcome.name, func(t *testing.T) {
				control := &f11WriteOffUpdateControl{t: t, statement: update.sql, arguments: update.arguments, tag: pgconn.NewCommandTag(outcome.rows), err: outcome.err}
				err := receivableWriteOffGuardedUpdate(context.Background(), control, update.sql, update.arguments...)
				if control.calls != 1 || (outcome.err != nil && err != outcome.err) || (outcome.definite && !errors.Is(err, ErrConflict)) || (outcome.rows == "UPDATE 1" && err != nil) {
					t.Fatalf("guarded outcome calls=%d err=%v", control.calls, err)
				}
				owner := &writeOffExecutionStub{authorizationExecutionRepositoryStub: &authorizationExecutionRepositoryStub{activations: ApprovalCommandActivationSnapshot{SchemaVersion: 1, Activations: []ApprovalCommandActivation{{CommandType: receivableWriteOffCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1, State: "ACTIVE", Version: 4, UpdatedAt: now}}}}, runErr: err, result: ReceivableWriteOffResult{CaseID: uuid.New(), JournalEntryID: uuid.New(), Status: "SUCCEEDED"}}
				domain := &billingAuthorizationDomain{repository: owner}
				attempt := domain.executeReceivableWriteOff(context.Background(), binding, event, command)
				if owner.runs != 1 || owner.input.ApprovalRequestID != binding.ApprovalRequestID || owner.input.AuthorizationEventID != binding.EventID || owner.input.ExecutionKey != command.ExecutionIdempotencyKey || owner.input.Command.ReceivableID != plan.ReceivableID || attempt.FailureIsDefinite != outcome.definite {
					t.Fatalf("actual delivery changed owner tuple/classification: runs=%d input=%+v attempt=%+v", owner.runs, owner.input, attempt)
				}
				if outcome.err != nil && (attempt.StableFailureCode != "" || len(attempt.Result.DomainResult) != 0) {
					t.Fatal("typed storage error became definite denial or success")
				}
				if outcome.definite && attempt.StableFailureCode != "DOMAIN_PRECONDITION_REJECTED" {
					t.Fatal("completed zero-row guard lost its existing definite conflict")
				}
				if outcome.rows == "UPDATE 1" && len(attempt.Result.DomainResult) == 0 {
					t.Fatal("normal completed guard did not retain successful delivery")
				}
			})
		}
	}
}

type f11WriteOffRollbackControl struct {
	order *[]string
	err   error
}

func (control *f11WriteOffRollbackControl) Rollback(context.Context) error {
	*control.order = append(*control.order, "rollback")
	return control.err
}

func TestF11WriteOffUniqueRecoveryUsesRolledBackExactApproval(t *testing.T) {
	input := ReceivableWriteOffExecutionInput{ApprovalRequestID: uuid.New(), AuthorizationEventID: uuid.New(), ApprovalEpoch: 7,
		ExecutionKey: "write-off:original", Command: ReceivableWriteOffCommandInput{ReceivableID: uuid.New(), WriteOffMicrocredits: 10, ExpectedReceivableVersion: 2}, Frozen: ReceivableWriteOffFrozenFacts{FactsDigest: strings.Repeat("a", 64)}}
	stored := receivableWriteOffCaseRecord{ReceivableWriteOffResult: ReceivableWriteOffResult{CaseID: uuid.New(), ReceivableID: input.Command.ReceivableID, WalletID: uuid.New(), AmountMicrocredits: 10, ReceivableVersionBefore: 2, ReceivableVersionAfter: 3, StatusAfter: "PARTIALLY_SETTLED", FactsDigest: input.Frozen.FactsDigest, JournalEntryID: uuid.New(), AllocationID: uuid.New(), Status: "SUCCEEDED"}, ApprovalRequestID: input.ApprovalRequestID, AuthorizationEventID: input.AuthorizationEventID, ApprovalEpoch: 7, ExecutionKeyDigest: legacyFinancialExecutionKeyDigestV1(input.ExecutionKey)}
	variants := []struct {
		name   string
		mutate func(*receivableWriteOffCaseRecord)
	}{
		{"exact", func(*receivableWriteOffCaseRecord) {}},
		{"approval", func(r *receivableWriteOffCaseRecord) { r.ApprovalRequestID = uuid.New() }},
		{"event", func(r *receivableWriteOffCaseRecord) { r.AuthorizationEventID = uuid.New() }},
		{"epoch", func(r *receivableWriteOffCaseRecord) { r.ApprovalEpoch++ }},
		{"execution-key", func(r *receivableWriteOffCaseRecord) {
			r.ExecutionKeyDigest = legacyFinancialExecutionKeyDigestV1("different")
		}},
		{"receivable", func(r *receivableWriteOffCaseRecord) { r.ReceivableID = uuid.New() }},
		{"amount", func(r *receivableWriteOffCaseRecord) { r.AmountMicrocredits++ }},
		{"version", func(r *receivableWriteOffCaseRecord) { r.ReceivableVersionBefore++ }},
		{"facts", func(r *receivableWriteOffCaseRecord) { r.FactsDigest = strings.Repeat("b", 64) }},
	}
	for _, variant := range variants {
		t.Run(variant.name, func(t *testing.T) {
			order := []string{}
			candidate := stored
			variant.mutate(&candidate)
			// A different prior approval for the same partially settled receivable
			// is deliberately first in storage; only the requested ID may be read.
			prior := stored
			prior.ApprovalRequestID = uuid.New()
			read := func(_ context.Context, id uuid.UUID) (receivableWriteOffCaseRecord, error) {
				if !reflect.DeepEqual(order, []string{"rollback"}) {
					t.Fatal("attempted a read while the failed write transaction was still active")
				}
				order = append(order, "readonly exact request")
				if id != input.ApprovalRequestID {
					return prior, nil
				}
				return candidate, nil
			}
			result, err := recoverReceivableWriteOffUnique(context.Background(), &f11WriteOffRollbackControl{order: &order}, read, input, input.ExecutionKey)
			if !reflect.DeepEqual(order, []string{"rollback", "readonly exact request"}) {
				t.Fatal("recovery did not retain rollback-then-readonly ordering")
			}
			if variant.name == "exact" {
				if err != nil || result != stored.ReceivableWriteOffResult {
					t.Fatal("matching committed receipt was not returned unchanged", err)
				}
			} else if !errors.Is(err, ErrConflict) || result != (ReceivableWriteOffResult{}) {
				t.Fatal("different immutable tuple became this approval's successful receipt", err)
			}
		})
	}
	for _, failure := range []struct {
		name           string
		rollback, read error
	}{
		{"rollback", context.Canceled, nil},
		{"storage-read", nil, &pgconn.PgError{Code: "08006"}},
		{"readonly-commit", nil, &pgconn.PgError{Code: "40001"}},
		{"missing-receipt", nil, ErrNotFound},
	} {
		t.Run(failure.name, func(t *testing.T) {
			order := []string{}
			read := func(_ context.Context, id uuid.UUID) (receivableWriteOffCaseRecord, error) {
				if id != input.ApprovalRequestID || !reflect.DeepEqual(order, []string{"rollback"}) {
					t.Fatal("wrong recovery request/order")
				}
				order = append(order, "read")
				return receivableWriteOffCaseRecord{}, failure.read
			}
			result, err := recoverReceivableWriteOffUnique(context.Background(), &f11WriteOffRollbackControl{order: &order, err: failure.rollback}, read, input, input.ExecutionKey)
			if result != (ReceivableWriteOffResult{}) {
				t.Fatal("failed recovery manufactured a receipt")
			}
			if failure.rollback != nil {
				if err != failure.rollback || len(order) != 1 {
					t.Fatal("rollback failure did not stop all reads", err)
				}
			} else if failure.read == ErrNotFound {
				if !errors.Is(err, ErrConflict) {
					t.Fatal("confirmed immutable conflict changed", err)
				}
			} else if err != failure.read || classifyAuthorizationDomainFailure(err).FailureIsDefinite {
				t.Fatal("recovery infrastructure error became definite rejection", err)
			}
		})
	}
}
