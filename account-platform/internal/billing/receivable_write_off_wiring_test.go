package billing

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// These controlled owners exercise the installed production mapping; no
// provider, database, or NATS effects are inferred from this transport slice.
type writeOffPrecheckStub struct {
	billingApprovalPrecheckRepository
	input ReceivableWriteOffCommandInput
	calls int
}

func (s *writeOffPrecheckStub) ReceivableWriteOffProofInput(_ context.Context, input ReceivableWriteOffCommandInput) (approvalencoding.LegacyProofInput, error) {
	s.calls++
	s.input = input
	return approvalencoding.LegacyProofInput{CommandType: receivableWriteOffCommand, TargetResourceID: input.ReceivableID.String()}, nil
}

func TestReceivableWriteOffPrecheckClosedTransport(t *testing.T) {
	id := uuid.New()
	raw := `{"command_type":"RECEIVABLE_WRITE_OFF","target_resource_id":"` + id.String() + `","reason_code":"APPROVED_RECEIVABLE_WRITE_OFF","write_off_microcredits":9223372036854775807,"expected_version":9007199254740993}`
	owner := &writeOffPrecheckStub{}
	request, err := decodeBillingApprovalPrecheckRequest([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	result, err := precheckBillingApprovalCommand(context.Background(), owner, request)
	if err != nil || result.TargetResourceID != id.String() || owner.calls != 1 || owner.input.WriteOffMicrocredits != math.MaxInt64 || owner.input.ExpectedReceivableVersion != 9007199254740993 {
		t.Fatalf("int64 owner mapping: result=%+v input=%+v calls=%d err=%v", result, owner.input, owner.calls, err)
	}
	for name, body := range map[string]string{
		"unknown":   strings.TrimSuffix(raw, "}") + `,"financial_snapshot":{}}`,
		"duplicate": strings.TrimSuffix(raw, "}") + `,"expected_version":1}`,
		"trailing":  raw + `{}`,
		"float":     strings.Replace(raw, "9223372036854775807", "1.5", 1),
		"overflow":  strings.Replace(raw, "9223372036854775807", "9223372036854775808", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeBillingApprovalPrecheckRequest([]byte(body)); err == nil {
				t.Fatal("accepted noncontract JSON")
			}
		})
	}
	for name, change := range map[string]func(*billingApprovalPrecheckRequest){
		"reason":        func(r *billingApprovalPrecheckRequest) { r.ReasonCode += " " },
		"zero_amount":   func(r *billingApprovalPrecheckRequest) { r.WriteOffMicrocredits = 0 },
		"zero_version":  func(r *billingApprovalPrecheckRequest) { r.ExpectedVersion = 0 },
		"foreign_order": func(r *billingApprovalPrecheckRequest) { r.PaymentOrderID = uuid.NewString() },
		"future":        func(r *billingApprovalPrecheckRequest) { r.CommandType = "REFUND_EXCEPTION_REPAIR" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := request
			change(&bad)
			if _, err := precheckBillingApprovalCommand(context.Background(), owner, bad); !errors.Is(err, errBillingApprovalPrecheckUnavailable) {
				t.Fatalf("bad precheck err=%v", err)
			}
			if owner.calls != 1 {
				t.Fatal("invalid input reached owner")
			}
		})
	}
}

type writeOffFactsStub struct {
	billingProofFacts
	plan ReceivableWriteOffPlan
	err  error
}

type writeOffProofRepository struct {
	Repository
	*writeOffFactsStub
	snapshot           ApprovalCommandActivationSnapshot
	reads              int
	changeOnSecondRead bool
}

func (s *writeOffProofRepository) ApprovalCommandActivations(context.Context) (ApprovalCommandActivationSnapshot, error) {
	s.reads++
	result := ApprovalCommandActivationSnapshot{SchemaVersion: s.snapshot.SchemaVersion, Activations: append([]ApprovalCommandActivation(nil), s.snapshot.Activations...)}
	if s.changeOnSecondRead && s.reads == 2 {
		for i := range result.Activations {
			if result.Activations[i].CommandType == receivableWriteOffCommand {
				result.Activations[i].Version++
			}
		}
	}
	return result, nil
}

func TestReceivableWriteOffProofServiceActivation(t *testing.T) {
	_, event, _, plan := writeOffWireFixture()
	artifacts, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{CommandType: event.CommandType, PolicyVersion: 1, SnapshotSchemaVersion: 1, TargetResourceType: event.TargetResourceType, TargetResourceID: event.TargetResourceID, NormalizedCommand: event.NormalizedCommand, CriticalResourceVersions: event.CriticalResourceVersions, FinancialSnapshot: event.FinancialSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	request := approvalencoding.ProofRequest{ApprovalRequestID: uuid.New(), Artifacts: artifacts}
	for _, name := range []string{"active", "inactive", "changed_activation", "changed_owner"} {
		t.Run(name, func(t *testing.T) {
			snapshot := validBillingApprovalActivationSnapshot(time.Now().UTC())
			for i := range snapshot.Activations {
				if snapshot.Activations[i].CommandType == receivableWriteOffCommand && name != "inactive" {
					snapshot.Activations[i].State = "ACTIVE"
					snapshot.Activations[i].Version = 4
				}
			}
			owner := &writeOffProofRepository{writeOffFactsStub: &writeOffFactsStub{plan: plan}, snapshot: snapshot, changeOnSecondRead: name == "changed_activation"}
			if name == "changed_owner" {
				owner.plan.OutstandingMicrocredits++
				owner.plan.OriginalMicrocredits++
				owner.plan.WalletReceivableMicrocredits++
			}
			response, err := NewService(owner).billingApprovalProof(context.Background(), request)
			if name == "inactive" || name == "changed_activation" {
				if !errors.Is(err, errBillingApprovalProofUnavailable) {
					t.Fatalf("unexpected proof availability: %+v %v", response, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if name == "changed_owner" {
				if response.Verdict != "DENY" || response.ReasonCode != "SNAPSHOT_MISMATCH" {
					t.Fatalf("changed facts proof %+v", response)
				}
				return
			}
			if response.Verdict != "ALLOW" || !strings.HasPrefix(response.DomainRevision, "4:") || response.ApprovalRequestID != request.ApprovalRequestID || response.CommandDigest != artifacts.CommandDigest || response.SnapshotDigest != artifacts.SnapshotDigest {
				t.Fatalf("unexpected proof %+v", response)
			}
		})
	}
}

func (s *writeOffFactsStub) receivableWriteOffPlan(context.Context, ReceivableWriteOffCommandInput) (ReceivableWriteOffPlan, error) {
	return s.plan, s.err
}

func writeOffWireFixture() (ApprovalAuthorizationBinding, approvalencoding.AuthorizationEvent, ApprovedCommand, ReceivableWriteOffPlan) {
	plan := ReceivableWriteOffPlan{ReceivableID: uuid.New(), WalletID: uuid.New(), SourceType: "USAGE_OVERAGE", SourceReference: "usage:durable", ReceivableStatus: "PARTIALLY_SETTLED", OriginalMicrocredits: 50, SettledMicrocredits: 20, OutstandingMicrocredits: 30, ReceivableVersion: 9007199254740993, WalletReceivableMicrocredits: 30, WriteOffMicrocredits: 30, FactsDigest: strings.Repeat("a", 64)}
	normalized := marshalRaw(map[string]any{"receivable_id": plan.ReceivableID, "write_off_microcredits": plan.WriteOffMicrocredits, "expected_receivable_version": plan.ReceivableVersion, "reason_code": receivableWriteOffReason})
	critical, financial := receivableWriteOffProofProjection(plan)
	binding := ApprovalAuthorizationBinding{ApprovalRequestID: uuid.New(), EventID: uuid.New(), ApprovalEpoch: 5, PolicyVersion: 1, CommandType: receivableWriteOffCommand, ExecutorIdentity: billingApprovalExecutorIdentity}
	event := approvalencoding.AuthorizationEvent{CommandType: binding.CommandType, PolicyVersion: 1, ApprovalRequestID: binding.ApprovalRequestID, EventID: binding.EventID, ApprovalControlEpoch: binding.ApprovalEpoch, ExecutorIdentity: binding.ExecutorIdentity, TargetResourceType: "RECEIVABLE", TargetResourceID: plan.ReceivableID.String(), NormalizedCommand: normalized, CriticalResourceVersions: critical, FinancialSnapshot: financial}
	command := ApprovedCommand{CommandType: binding.CommandType, PolicyVersion: 1, ApprovalRequestID: binding.ApprovalRequestID, ExecutorIdentity: binding.ExecutorIdentity, TargetResourceType: event.TargetResourceType, TargetResourceID: event.TargetResourceID, NormalizedCommand: normalized, CriticalResourceVersions: critical, FinancialSnapshot: financial, RequesterUserID: uuid.New(), ApproverUserID: uuid.New(), DecisionReason: "independent review", ExecutionIdempotencyKey: "write-off:stable"}
	return binding, event, command, plan
}

func TestReceivableWriteOffProofOwnerProjection(t *testing.T) {
	_, event, _, plan := writeOffWireFixture()
	frozen := approvalencoding.LegacyProofInput{CommandType: receivableWriteOffCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1, TargetResourceType: event.TargetResourceType, TargetResourceID: event.TargetResourceID, NormalizedCommand: event.NormalizedCommand}
	owner := &writeOffFactsStub{plan: plan}
	current, reason, state, err := currentBillingProofInput(context.Background(), owner, frozen)
	if err != nil || reason != "" || state != "USAGE_OVERAGE:PARTIALLY_SETTLED" || !sameRawJSON(current.CriticalResourceVersions, event.CriticalResourceVersions) || !sameRawJSON(current.FinancialSnapshot, event.FinancialSnapshot) {
		t.Fatalf("owner projection: %s/%s %v %+v", reason, state, err, current)
	}
	for _, code := range []string{"", "OTHER", " " + receivableWriteOffReason, receivableWriteOffReason + " "} {
		bad := frozen
		bad.NormalizedCommand = marshalRaw(map[string]any{"receivable_id": plan.ReceivableID, "write_off_microcredits": 30, "expected_receivable_version": plan.ReceivableVersion, "reason_code": code})
		if _, _, _, err := currentBillingProofInput(context.Background(), owner, bad); !errors.Is(err, ErrApprovalSnapshotMismatch) {
			t.Fatalf("reason %q err=%v", code, err)
		}
	}
	owner.plan.WriteOffMicrocredits++
	if _, reason, _, err := currentBillingProofInput(context.Background(), owner, frozen); err != nil || reason != "SNAPSHOT_MISMATCH" {
		t.Fatalf("changed owner reason=%s err=%v", reason, err)
	}
	owner.err = ErrNotFound
	if _, reason, _, err := currentBillingProofInput(context.Background(), owner, frozen); err != nil || reason != "DOMAIN_INVARIANT_FAILED" {
		t.Fatalf("missing owner reason=%s err=%v", reason, err)
	}
	owner.err = errors.New("owner read unavailable")
	if _, _, _, err := currentBillingProofInput(context.Background(), owner, frozen); !errors.Is(err, owner.err) {
		t.Fatalf("infra became definite denial: %v", err)
	}
}

type writeOffExecutionStub struct {
	*authorizationExecutionRepositoryStub
	result  ReceivableWriteOffResult
	readErr error
	runErr  error
	input   ReceivableWriteOffExecutionInput
	runs    int
}

func (s *writeOffExecutionStub) ReceivableWriteOffForRequest(context.Context, uuid.UUID) (ReceivableWriteOffResult, error) {
	return s.result, s.readErr
}
func (s *writeOffExecutionStub) ApplyReceivableWriteOff(_ context.Context, input ReceivableWriteOffExecutionInput) (ReceivableWriteOffResult, error) {
	s.runs++
	s.input = input
	return s.result, s.runErr
}

func TestReceivableWriteOffAuthorizationProofClaimAndResult(t *testing.T) {
	binding, event, command, plan := writeOffWireFixture()
	artifacts, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{CommandType: event.CommandType, PolicyVersion: 1, SnapshotSchemaVersion: 1, TargetResourceType: event.TargetResourceType, TargetResourceID: event.TargetResourceID, NormalizedCommand: event.NormalizedCommand, CriticalResourceVersions: event.CriticalResourceVersions, FinancialSnapshot: event.FinancialSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	event.ProofSchemaVersion = approvalencoding.ProofSchemaVersion
	event.CommandDigest, event.SnapshotDigest, event.DomainRevision = artifacts.CommandDigest, artifacts.SnapshotDigest, "4:"+strings.Repeat("b", 64)
	binding.SerializationVersion = approvalencoding.ApprovalSerializationVersion
	owner := &writeOffExecutionStub{authorizationExecutionRepositoryStub: &authorizationExecutionRepositoryStub{activations: ApprovalCommandActivationSnapshot{SchemaVersion: 1, Activations: []ApprovalCommandActivation{{CommandType: receivableWriteOffCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1, State: "ACTIVE", Version: 4, UpdatedAt: time.Now().UTC()}}}}, result: ReceivableWriteOffResult{CaseID: uuid.New(), JournalEntryID: uuid.New(), Status: "SUCCEEDED"}}
	proofs, claims := 0, 0
	domain := &billingAuthorizationDomain{repository: owner, database: &authorizationQueryStub{t: t}, priorClaim: func(context.Context, ApprovalAuthorizationBinding) (bool, error) { return false, nil }, proof: func(_ context.Context, request approvalencoding.ProofRequest) (approvalencoding.ProofResponse, error) {
		proofs++
		if request.Artifacts.CommandDigest != artifacts.CommandDigest || request.Artifacts.SnapshotSchemaVersion != 1 {
			t.Fatal("wrong proof contract")
		}
		return approvalencoding.ProofResponse{Verdict: "ALLOW", DomainRevision: event.DomainRevision, CommandDigest: event.CommandDigest, SnapshotDigest: event.SnapshotDigest}, nil
	}, claim: func(context.Context, ApprovalAuthorizationBinding, approvalencoding.AuthorizationEvent) error {
		claims++
		return nil
	}}
	attempt := domain.ExecuteAuthorizationAttempt(context.Background(), binding, event, command)
	if attempt.FailureIsDefinite || len(attempt.Result.DomainResult) == 0 || proofs != 1 || claims != 1 || owner.runs != 1 || owner.input.Command.ReceivableID != plan.ReceivableID || owner.input.Command.ExpectedReceivableVersion != plan.ReceivableVersion || owner.input.ActivationVersion != 4 || owner.input.Frozen.FactsDigest != plan.FactsDigest || owner.input.AuthorizationEventID != binding.EventID || owner.input.ApprovalEpoch != binding.ApprovalEpoch || owner.input.ExecutionKey != command.ExecutionIdempotencyKey {
		t.Fatalf("mapping=%+v input=%+v proof=%d claim=%d", attempt, owner.input, proofs, claims)
	}
	// Persisted read is authoritative without consulting a now-disabled owner.
	owner.activations = ApprovalCommandActivationSnapshot{}
	result, found, err := domain.LookupAuthorizationResult(context.Background(), binding, event)
	if err != nil || !found || !strings.Contains(string(result.DomainResult), owner.result.CaseID.String()) || !strings.Contains(string(result.LedgerReferences), owner.result.JournalEntryID.String()) || owner.runs != 1 || proofs != 1 {
		t.Fatalf("result-first=%+v found=%v err=%v", result, found, err)
	}
	owner.readErr = ErrNotFound
	if _, found, err := domain.LookupAuthorizationResult(context.Background(), binding, event); err != nil || found {
		t.Fatalf("absence found=%v err=%v", found, err)
	}
	owner.readErr = errors.New("read unavailable")
	if _, found, err := domain.LookupAuthorizationResult(context.Background(), binding, event); err == nil || found {
		t.Fatal("infra read became absence")
	}
	// Direct mapping controls isolate the post-claim effect boundary.
	for name, change := range map[string]func(*approvalencoding.AuthorizationEvent, *ApprovedCommand){
		"target": func(e *approvalencoding.AuthorizationEvent, _ *ApprovedCommand) {
			e.TargetResourceID = uuid.NewString()
		},
		"epoch":      func(e *approvalencoding.AuthorizationEvent, _ *ApprovedCommand) { e.ApprovalControlEpoch++ },
		"same_actor": func(_ *approvalencoding.AuthorizationEvent, c *ApprovedCommand) { c.ApproverUserID = c.RequesterUserID },
		"financial": func(e *approvalencoding.AuthorizationEvent, c *ApprovedCommand) {
			e.FinancialSnapshot = json.RawMessage(`{}`)
			c.FinancialSnapshot = e.FinancialSnapshot
		},
	} {
		t.Run(name, func(t *testing.T) {
			e, c := event, command
			change(&e, &c)
			bad := domain.executeReceivableWriteOff(context.Background(), binding, e, c)
			if !bad.FailureIsDefinite || owner.runs != 1 {
				t.Fatalf("tamper=%+v runs=%d", bad, owner.runs)
			}
		})
	}
	bad := domain.executeReceivableWriteOff(context.Background(), binding, event, command)
	if !bad.FailureIsDefinite || bad.StableFailureCode != "POLICY_OR_SCHEMA_MISMATCH" || owner.runs != 1 {
		t.Fatalf("inactive=%+v", bad)
	}
}

type writeOffActivationReader struct {
	receivableWriteOffReader
	item ApprovalCommandActivation
	err  error
	t    *testing.T
}

type writeOffClaimTx struct {
	pgx.Tx
	t     *testing.T
	state string
	err   error
}

func (s *writeOffClaimTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if !strings.Contains(sql, "billing_approval_command_activations") || len(args) != 1 || args[0] != receivableWriteOffCommand {
		s.t.Fatal("rejected claim reached owner plan")
	}
	return s
}
func (s *writeOffClaimTx) Scan(dest ...any) error {
	if s.err != nil {
		return s.err
	}
	*dest[0].(*int64), *dest[1].(*int64), *dest[2].(*string), *dest[3].(*int64) = 1, 1, s.state, 4
	return nil
}
func TestReceivableWriteOffClaimFailureClassification(t *testing.T) {
	for _, name := range []string{"inactive", "missing", "infra"} {
		t.Run(name, func(t *testing.T) {
			tx := &writeOffClaimTx{t: t, state: "INACTIVE"}
			if name == "missing" {
				tx.err = pgx.ErrNoRows
			}
			if name == "infra" {
				tx.err = errors.New("activation read unavailable")
			}
			_, err := (billingTxProofFacts{tx: tx}).receivableWriteOffPlan(context.Background(), ReceivableWriteOffCommandInput{})
			if name == "infra" {
				if !errors.Is(err, tx.err) || errors.Is(err, ErrApprovalSnapshotMismatch) {
					t.Fatalf("infra turned definite: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrApprovalSnapshotMismatch) {
				t.Fatalf("claim gate=%v", err)
			}
		})
	}
}

func (s *writeOffActivationReader) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if !strings.Contains(sql, "billing_approval_execution.lock_receivable_activation()") || len(args) != 0 {
		s.t.Fatalf("wrong owner activation query %v", args)
	}
	return s
}
func (s *writeOffActivationReader) Scan(dest ...any) error {
	if s.err != nil {
		return s.err
	}
	*dest[0].(*bool) = s.item.State == "ACTIVE"
	*dest[1].(*int64) = s.item.Version
	*dest[2].(*time.Time) = s.item.UpdatedAt
	return nil
}

func TestReceivableWriteOffFreshActivationIsClosed(t *testing.T) {
	now := time.Now().UTC()
	valid := ApprovalCommandActivation{CommandType: receivableWriteOffCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1, State: "ACTIVE", Version: 4, UpdatedAt: now}
	input := ReceivableWriteOffExecutionInput{PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: 4}
	for name, change := range map[string]func(*writeOffActivationReader){
		"active":      func(*writeOffActivationReader) {},
		"inactive":    func(s *writeOffActivationReader) { s.item.State = "INACTIVE" },
		"version":     func(s *writeOffActivationReader) { s.item.Version++ },
		"schema":      func(s *writeOffActivationReader) { s.item.SnapshotSchemaVersion++ },
		"policy":      func(s *writeOffActivationReader) { s.item.PolicyVersion++ },
		"future_time": func(s *writeOffActivationReader) { s.item.UpdatedAt = now.Add(2 * time.Second) },
		"missing":     func(s *writeOffActivationReader) { s.err = pgx.ErrNoRows },
		"infra":       func(s *writeOffActivationReader) { s.err = errors.New("database unavailable") },
	} {
		t.Run(name, func(t *testing.T) {
			reader := &writeOffActivationReader{item: valid, t: t}
			change(reader)
			invocation := input
			// The owner function has a fixed tuple; these negatives belong to the caller's frozen tuple.
			if name == "schema" {
				invocation.SchemaVersion++
			}
			if name == "policy" {
				invocation.PolicyVersion++
			}
			err := receivableWriteOffExecutionActivation(context.Background(), reader, invocation, now)
			if name == "active" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if name == "infra" {
				if !errors.Is(err, reader.err) {
					t.Fatalf("infra classification %v", err)
				}
				return
			}
			if !errors.Is(err, ErrApprovalSnapshotMismatch) {
				t.Fatalf("bad activation %v", err)
			}
		})
	}
	snapshot := validBillingApprovalActivationSnapshot(now)
	for i := range snapshot.Activations {
		if snapshot.Activations[i].CommandType == receivableWriteOffCommand {
			snapshot.Activations[i] = valid
		}
	}
	if err := validateApprovalCommandActivationSnapshot(snapshot, now); err != nil {
		t.Fatal(err)
	}
	for i := range snapshot.Activations {
		if snapshot.Activations[i].CommandType == "REFUND_EXCEPTION_REPAIR" {
			snapshot.Activations[i].State = "ACTIVE"
		}
	}
	if err := validateApprovalCommandActivationSnapshot(snapshot, now); !errors.Is(err, ErrApprovalActivationUnavailable) {
		t.Fatal("future activation allowed")
	}
}
