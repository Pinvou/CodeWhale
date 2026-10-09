package billing

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"os"
	"testing"
	"time"
)

func TestPrivateF11OwnerFrozenFailureAndRollback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	repo, _ := newAdminPaginationIntegrationRepository(t, ctx, os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	pool := repo.pool
	now := time.Now().UTC()
	wid, rid := uuid.New(), uuid.New()
	if _, e := pool.Exec(ctx, `INSERT INTO wallets(id,owner_type,owner_id,status,posted_microcredits,receivable_microcredits,financial_status,financial_freeze_reason,version,created_at,updated_at) VALUES($1,'PERSONAL',$2,'ACTIVE',500,50,'RECEIVABLE_OPEN','USAGE_OVERAGE',1,$3,$3)`, wid, uuid.New(), now); e != nil {
		t.Fatal(e)
	}
	if _, e := pool.Exec(ctx, `INSERT INTO receivables(id,wallet_id,source_type,source_reference,original_microcredits,outstanding_microcredits,status,created_at,updated_at) VALUES($1,$2,'USAGE_OVERAGE',$3,50,50,'OPEN',$4,$4)`, rid, wid, "usage:"+rid.String(), now); e != nil {
		t.Fatal(e)
	}
	command := ReceivableWriteOffCommandInput{ReceivableID: rid, WriteOffMicrocredits: 20, ExpectedReceivableVersion: 1}
	p, e := repo.receivableWriteOffPlanFrom(ctx, command, 1, 1, 1)
	if e != nil {
		t.Fatal(e)
	}
	input := ReceivableWriteOffExecutionInput{ApprovalRequestID: uuid.New(), AuthorizationEventID: uuid.New(), ApprovalEpoch: 1, ExecutionKey: "private-f11-" + rid.String(), PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: 1, Command: command, Frozen: ReceivableWriteOffFrozenFacts{ReceivableVersion: p.ReceivableVersion, WriteOffMicrocredits: p.WriteOffMicrocredits, OutstandingMicrocredits: p.OutstandingMicrocredits, SettledMicrocredits: p.SettledMicrocredits, OriginalMicrocredits: p.OriginalMicrocredits, SourceType: p.SourceType, SourceReference: p.SourceReference, WalletReceivableMicrocredits: p.WalletReceivableMicrocredits, FactsDigest: p.FactsDigest}}
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
	}}, {"stale-target-version", func(i *ReceivableWriteOffExecutionInput) { i.Command.ExpectedReceivableVersion++ }}} {
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
	t.Run("chargeback-source-cannot-writeoff", func(t *testing.T) {
		id := uuid.New()
		if _, e := pool.Exec(ctx, `INSERT INTO receivables(id,wallet_id,source_type,source_reference,original_microcredits,outstanding_microcredits,status,created_at,updated_at) VALUES($1,$2,'CHARGEBACK',$3,10,10,'OPEN',$4,$4)`, id, wid, "chargeback:"+id.String(), now); e != nil {
			t.Fatal("legal fixture source", e)
		}
		before := snapshot()
		if _, e := repo.receivableWriteOffPlanFrom(ctx, ReceivableWriteOffCommandInput{ReceivableID: id, WriteOffMicrocredits: 1, ExpectedReceivableVersion: 1}, 1, 1, 1); !errors.Is(e, ErrApprovalSnapshotMismatch) {
			t.Fatal("foreign source accepted", e)
		}
		if snapshot() != before {
			t.Fatal("source rejection mutated effect")
		}
	})
}
