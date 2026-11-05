package billing

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"os"
	"testing"
	"time"
)

func TestPrivateF11OwnerFrozenFailureAndRollback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	repo, _ := newAdminPaginationIntegrationRepository(t, ctx, os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	pool := repo.pool
	planFrom := func(command ReceivableWriteOffCommandInput, policy, schema, activation int64) (ReceivableWriteOffPlan, error) {
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly})
		if err != nil {
			return ReceivableWriteOffPlan{}, err
		}
		defer tx.Rollback(ctx)
		return receivableWriteOffPlanFrom(ctx, tx, command, policy, schema, activation)
	}
	setActivation := writeOffFixtureActivation(t, ctx, pool)
	activationVersion := setActivation("ACTIVE")
	now := time.Now().UTC()
	wid, rid := uuid.New(), uuid.New()
	if _, e := pool.Exec(ctx, `INSERT INTO wallets(id,owner_type,owner_id,status,posted_microcredits,receivable_microcredits,financial_status,financial_freeze_reason,version,created_at,updated_at) VALUES($1,'PERSONAL',$2,'ACTIVE',500,50,'RECEIVABLE_OPEN','USAGE_OVERAGE',1,$3,$3)`, wid, uuid.New(), now); e != nil {
		t.Fatal(e)
	}
	if _, e := pool.Exec(ctx, `INSERT INTO receivables(id,wallet_id,source_type,source_reference,original_microcredits,outstanding_microcredits,status,created_at,updated_at) VALUES($1,$2,'USAGE_OVERAGE',$3,50,50,'OPEN',$4,$4)`, rid, wid, "usage:"+rid.String(), now); e != nil {
		t.Fatal(e)
	}
	command := ReceivableWriteOffCommandInput{ReceivableID: rid, WriteOffMicrocredits: 20, ExpectedReceivableVersion: 1}
	p, e := planFrom(command, 1, 1, activationVersion)
	if e != nil {
		t.Fatal(e)
	}
	input := ReceivableWriteOffExecutionInput{ApprovalRequestID: uuid.New(), AuthorizationEventID: uuid.New(), ApprovalEpoch: 1, ExecutionKey: "private-f11-" + rid.String(), PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: activationVersion, Command: command, Frozen: ReceivableWriteOffFrozenFacts{ReceivableVersion: p.ReceivableVersion, WriteOffMicrocredits: p.WriteOffMicrocredits, OutstandingMicrocredits: p.OutstandingMicrocredits, SettledMicrocredits: p.SettledMicrocredits, OriginalMicrocredits: p.OriginalMicrocredits, SourceType: p.SourceType, SourceReference: p.SourceReference, WalletReceivableMicrocredits: p.WalletReceivableMicrocredits, FactsDigest: p.FactsDigest}}
	snapshot := func() string {
		var raw string
		if e := pool.QueryRow(ctx, `SELECT jsonb_build_array((SELECT to_jsonb(w) FROM wallets w WHERE id=$1),(SELECT to_jsonb(r) FROM receivables r WHERE id=$2),(SELECT count(*) FROM receivable_write_off_cases),(SELECT count(*) FROM journal_entries),(SELECT count(*) FROM ledger_postings),(SELECT count(*) FROM receivable_allocations),(SELECT count(*) FROM credit_lots))::text`, wid, rid).Scan(&raw); e != nil {
			t.Fatal(e)
		}
		return raw
	}
	for _, tc := range []struct {
		name string
		edit func(*ReceivableWriteOffExecutionInput)
	}{{"wrong-frozen-wallet", func(i *ReceivableWriteOffExecutionInput) { i.Frozen.WalletReceivableMicrocredits++ }}, {"wrong-owner-digest", func(i *ReceivableWriteOffExecutionInput) {
		i.Frozen.FactsDigest = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	}}, {"empty-frozen-digest", func(i *ReceivableWriteOffExecutionInput) { i.Frozen.FactsDigest = "" }}, {"stale-target-version", func(i *ReceivableWriteOffExecutionInput) { i.Command.ExpectedReceivableVersion++ }}} {
		t.Run(tc.name, func(t *testing.T) {
			before := snapshot()
			bad := input
			tc.edit(&bad)
			if _, e := repo.ApplyReceivableWriteOff(ctx, bad); !errors.Is(e, ErrApprovalSnapshotMismatch) {
				t.Fatal("frozen rejection", e)
			}
			if snapshot() != before {
				t.Fatal("definite rejection mutated domain")
			}
		})
	}
	t.Run("valid-effect-and-samekey-conflict-rollback", func(t *testing.T) {
		good, e := repo.ApplyReceivableWriteOff(ctx, input)
		if e != nil {
			t.Fatal(e)
		}
		before := snapshot()
		again, e := repo.ApplyReceivableWriteOff(ctx, input)
		if e != nil || again != good || snapshot() != before {
			t.Fatal("strict replay changed effect", e)
		}
		bad := input
		bad.Command.WriteOffMicrocredits++
		if _, e = repo.ApplyReceivableWriteOff(ctx, bad); !errors.Is(e, ErrConflict) || snapshot() != before {
			t.Fatal("changed samekey was not zero-write conflict", e)
		}
		if _, e = pool.Exec(ctx, `UPDATE receivable_write_off_cases SET amount_microcredits=amount_microcredits+1 WHERE approval_request_id=$1`, input.ApprovalRequestID); e == nil {
			t.Fatal("runtime mutated immutable case")
		}
		if snapshot() != before {
			t.Fatal("case reject altered facts")
		}
		b, _ := json.Marshal(good)
		if len(b) == 0 {
			t.Fatal("committed result missing")
		}
	})
	t.Run("proof-claim-wallet-version-drift-zero-effect", func(t *testing.T) {
		command := ReceivableWriteOffCommandInput{ReceivableID: rid, WriteOffMicrocredits: 1, ExpectedReceivableVersion: 2}
		plan, err := planFrom(command, 1, 1, activationVersion)
		if err != nil {
			t.Fatal(err)
		}
		critical, financial := receivableWriteOffProofProjection(plan)
		normalized := marshalRaw(map[string]any{"receivable_id": rid, "write_off_microcredits": command.WriteOffMicrocredits, "expected_receivable_version": command.ExpectedReceivableVersion, "reason_code": receivableWriteOffReason})
		artifacts, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{CommandType: receivableWriteOffCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1, TargetResourceType: "RECEIVABLE", TargetResourceID: rid.String(), NormalizedCommand: normalized, CriticalResourceVersions: critical, FinancialSnapshot: financial})
		if err != nil {
			t.Fatal(err)
		}
		requestID, eventID := uuid.New(), uuid.New()
		service := NewService(repo)
		proof, err := service.billingApprovalProof(ctx, approvalencoding.ProofRequest{ApprovalRequestID: requestID, Artifacts: artifacts})
		if err != nil || proof.Verdict != "ALLOW" {
			t.Fatalf("real owner proof=%+v err=%v", proof, err)
		}
		binding := ApprovalAuthorizationBinding{ApprovalRequestID: requestID, EventID: eventID, ApprovalEpoch: 1, CommandType: receivableWriteOffCommand, PolicyVersion: 1, ExecutorIdentity: billingApprovalExecutorIdentity, SerializationVersion: approvalencoding.ApprovalSerializationVersion}
		event := approvalencoding.AuthorizationEvent{ApprovalRequestID: requestID, EventID: eventID, ApprovalControlEpoch: 1, CommandType: receivableWriteOffCommand, PolicyVersion: 1, ExecutorIdentity: billingApprovalExecutorIdentity, TargetResourceType: "RECEIVABLE", TargetResourceID: rid.String(), NormalizedCommand: normalized, CriticalResourceVersions: critical, FinancialSnapshot: financial, ProofSchemaVersion: approvalencoding.ProofSchemaVersion, CommandDigest: artifacts.CommandDigest, SnapshotDigest: artifacts.SnapshotDigest, DomainRevision: proof.DomainRevision}
		approved := ApprovedCommand{ApprovalRequestID: requestID, CommandType: receivableWriteOffCommand, PolicyVersion: 1, ExecutorIdentity: billingApprovalExecutorIdentity, TargetResourceType: "RECEIVABLE", TargetResourceID: rid.String(), NormalizedCommand: normalized, CriticalResourceVersions: critical, FinancialSnapshot: financial, RequesterUserID: uuid.New(), ApproverUserID: uuid.New(), DecisionReason: "independent frozen review", ExecutionIdempotencyKey: "write-off:post-claim-drift"}
		domain := newBillingAuthorizationDomain(service).(*billingAuthorizationDomain)
		claim := domain.claim
		var afterDrift string
		domain.claim = func(ctx context.Context, b ApprovalAuthorizationBinding, e approvalencoding.AuthorizationEvent) error {
			if err := claim(ctx, b, e); err != nil {
				return err
			}
			// A real committed proof claim precedes the separate owner effect
			// transaction. Only the wallet version changes; all amounts remain.
			if _, err := pool.Exec(ctx, `UPDATE wallets SET version=version+1 WHERE id=$1`, wid); err != nil {
				return err
			}
			afterDrift = snapshot()
			return nil
		}
		attempt := domain.ExecuteAuthorizationAttempt(ctx, binding, event, approved)
		if afterDrift == "" {
			t.Fatalf("real proof/claim path not reached: %+v", attempt)
		}
		if !attempt.FailureIsDefinite || attempt.StableFailureCode != "DOMAIN_PRECONDITION_REJECTED" || len(attempt.Result.DomainResult) != 0 {
			t.Fatalf("stale same-amount facts accepted: %+v", attempt)
		}
		if snapshot() != afterDrift {
			t.Fatal("stale proof changed owner effects")
		}
		var claims int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM billing_approval_proof_claims WHERE approval_request_id=$1 AND authorization_event_id=$2`, requestID, eventID).Scan(&claims); err != nil || claims != 1 {
			t.Fatalf("real immutable claim=%d err=%v", claims, err)
		}
	})
	t.Run("chargeback-source-cannot-writeoff", func(t *testing.T) {
		id := uuid.New()
		if _, e := pool.Exec(ctx, `INSERT INTO receivables(id,wallet_id,source_type,source_reference,original_microcredits,outstanding_microcredits,status,created_at,updated_at) VALUES($1,$2,'CHARGEBACK',$3,10,10,'OPEN',$4,$4)`, id, wid, "chargeback:"+id.String(), now); e != nil {
			t.Fatal("legal fixture source", e)
		}
		before := snapshot()
		if _, e := planFrom(ReceivableWriteOffCommandInput{ReceivableID: id, WriteOffMicrocredits: 1, ExpectedReceivableVersion: 1}, 1, 1, 1); !errors.Is(e, ErrApprovalSnapshotMismatch) {
			t.Fatal("foreign source accepted", e)
		}
		if snapshot() != before {
			t.Fatal("source rejection mutated effect")
		}
	})
}
