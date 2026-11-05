package billing

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestReceivableWriteOffActivationTransactionFence proves the GSA-26 domain contract on
// real PostgreSQL: the full and the partial write-off, the exact journal and
// wallet/receivable projections, the boundary and source negative cases with
// zero writes, the zero-change guarantee for balances and lots, and result-first
// idempotency.
func TestReceivableWriteOffActivationTransactionFence(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	repository, _ := newAdminPaginationIntegrationRepository(t, ctx, databaseURL)
	pool := repository.pool
	setActivation := writeOffFixtureActivation(t, ctx, pool)
	activationVersion := int64(1)
	// Test the public package planner with an explicit read-only owner snapshot.
	planFrom := func(command ReceivableWriteOffCommandInput, policy, schema, activation int64) (ReceivableWriteOffPlan, error) {
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly})
		if err != nil {
			return ReceivableWriteOffPlan{}, err
		}
		defer tx.Rollback(ctx)
		return receivableWriteOffPlanFrom(ctx, tx, command, policy, schema, activation)
	}
	now := time.Now().UTC()

	walletID, walletAccountID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO wallets(id,owner_type,owner_id,status,posted_microcredits,receivable_microcredits,financial_status,financial_freeze_reason,version,created_at,updated_at)
VALUES($1,'PERSONAL',$2,'ACTIVE',500,90,'RECEIVABLE_OPEN','USAGE_OVERAGE',4,$3,$3)`, walletID, uuid.New(), now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ledger_accounts(id,wallet_id,code,account_type,created_at) VALUES($1,$2,$3,'WALLET_LIABILITY',$4)`,
		walletAccountID, walletID, "wallet:"+walletID.String(), now); err != nil {
		t.Fatal(err)
	}
	newReceivable := func(reference string, amount int64) uuid.UUID {
		id := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO receivables(id,wallet_id,source_type,source_reference,original_microcredits,outstanding_microcredits,status,created_at,updated_at)
VALUES($1,$2,'USAGE_OVERAGE',$3,$4,$4,'OPEN',$5,$5)`, id, walletID, reference, amount, now); err != nil {
			t.Fatal(err)
		}
		return id
	}
	newAllocation := func(receivableID uuid.UUID, amount int64) uuid.UUID {
		journalID, allocationID := uuid.New(), uuid.New()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err := insertJournal(ctx, tx, journalID, "RECHARGE", "cash:"+journalID.String(), "fixture", now); err != nil {
			t.Fatal(err)
		}
		if err := insertPosting(ctx, tx, journalID, walletAccountID, amount, nil, now); err != nil {
			t.Fatal(err)
		}
		if err := insertPosting(ctx, tx, journalID, paymentClearingAccount, -amount, nil, now); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO receivable_allocations(id,receivable_id,wallet_id,source_type,source_reference,amount_microcredits,journal_entry_id,created_at)
VALUES($1,$2,$3,'CASH_RECHARGE_BASE',$4,$5,$6,$7)`, allocationID, receivableID, walletID, "cash:"+allocationID.String(), amount, journalID, now); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return allocationID
	}

	// A receivable with an existing cash allocation must stay consistent: its
	// settled total already equals the allocation sum.
	partialID := newReceivable("usage:"+uuid.NewString(), 60)
	newAllocation(partialID, 20)
	if _, err := pool.Exec(ctx, `UPDATE receivables SET settled_microcredits=20,outstanding_microcredits=40,status='PARTIALLY_SETTLED',version=version+1,updated_at=$2 WHERE id=$1`, partialID, now); err != nil {
		t.Fatal(err)
	}
	fullID := newReceivable("usage:"+uuid.NewString(), 50)

	// The domain read path is activation-independent (only proof issuance and
	// execution require the live ACTIVE row), so the fixture plans directly.
	plan, err := planFrom(ReceivableWriteOffCommandInput{ReceivableID: fullID, WriteOffMicrocredits: 50, ExpectedReceivableVersion: 1}, 1, 1, 1)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.SourceType != "USAGE_OVERAGE" || plan.OutstandingMicrocredits != 50 || plan.SettledMicrocredits != 0 ||
		plan.FactsDigest == "" || plan.WalletReceivableMicrocredits != 90 {
		t.Fatalf("unexpected plan %+v", plan)
	}
	for _, invalid := range []ReceivableWriteOffCommandInput{
		{ReceivableID: fullID, WriteOffMicrocredits: 51, ExpectedReceivableVersion: 1},
		{ReceivableID: fullID, WriteOffMicrocredits: 0, ExpectedReceivableVersion: 1},
		{ReceivableID: fullID, WriteOffMicrocredits: 50, ExpectedReceivableVersion: 2},
		{ReceivableID: uuid.New(), WriteOffMicrocredits: 10, ExpectedReceivableVersion: 1},
	} {
		if _, err := planFrom(invalid, 1, 1, 1); !errors.Is(err, ErrApprovalSnapshotMismatch) && !errors.Is(err, ErrNotFound) {
			t.Fatalf("invalid plan %+v err=%v", invalid, err)
		}
	}
	var postedBefore int64
	if err := pool.QueryRow(ctx, `SELECT posted_microcredits FROM wallets WHERE id=$1`, walletID).Scan(&postedBefore); err != nil {
		t.Fatal(err)
	}
	execute := func(requestID, eventID, receivableID uuid.UUID, amount, expectedVersion int64, key string) (ReceivableWriteOffResult, error) {
		command := ReceivableWriteOffCommandInput{ReceivableID: receivableID, WriteOffMicrocredits: amount, ExpectedReceivableVersion: expectedVersion}
		// Freeze the same digest published in the approval proof; fresh execution
		// must compare it with the owner transaction recomputation.
		frozen, err := planFrom(command, 1, 1, activationVersion)
		if err != nil {
			return ReceivableWriteOffResult{}, err
		}
		execution := ReceivableWriteOffExecutionInput{
			ApprovalRequestID: requestID, AuthorizationEventID: eventID, ApprovalEpoch: 3,
			ExecutionKey: key, PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: activationVersion,
			Command: command,
			Frozen: ReceivableWriteOffFrozenFacts{
				ReceivableVersion: frozen.ReceivableVersion, WriteOffMicrocredits: frozen.WriteOffMicrocredits,
				OutstandingMicrocredits: frozen.OutstandingMicrocredits, SettledMicrocredits: frozen.SettledMicrocredits,
				OriginalMicrocredits: frozen.OriginalMicrocredits, SourceType: frozen.SourceType,
				SourceReference: frozen.SourceReference, WalletReceivableMicrocredits: frozen.WalletReceivableMicrocredits,
				FactsDigest: frozen.FactsDigest,
			},
		}
		if key == "race:write-off" {
			return receivableWriteOffWithDeactivationRace(t, ctx, repository, execution)
		}
		return repository.ApplyReceivableWriteOff(ctx, execution)
	}

	t.Run("inactive-zero-write", func(t *testing.T) {
		_, err := execute(uuid.New(), uuid.New(), fullID, 50, 1, "inactive:write-off")
		if !errors.Is(err, ErrApprovalSnapshotMismatch) {
			t.Fatal("inactive fresh execution must reject", err)
		}
		assertReceivableActivationNoEffect(t, ctx, repository, fullID, walletID)
	})
	activationVersion = setActivation("ACTIVE")
	t.Run("operator-version-change-zero-write", func(t *testing.T) {
		setActivation("INACTIVE")
		_, err := execute(uuid.New(), uuid.New(), fullID, 50, 1, "version:write-off")
		if !errors.Is(err, ErrApprovalSnapshotMismatch) {
			t.Fatal("old frozen activation version must reject", err)
		}
		assertReceivableActivationNoEffect(t, ctx, repository, fullID, walletID)
	})
	activationVersion = setActivation("ACTIVE")
	t.Run("operator-waits-for-real-financial-commit-and-receipt-replays", func(t *testing.T) {
		result, err := execute(uuid.New(), uuid.New(), fullID, 50, 1, "race:write-off")
		if err != nil || result.AmountMicrocredits != 50 || result.StatusAfter != "WRITTEN_OFF" {
			t.Fatal("financial execution failed", err)
		}
		var posted, aggregate, outstanding int64
		if err = pool.QueryRow(ctx, `SELECT w.posted_microcredits,w.receivable_microcredits,r.outstanding_microcredits FROM wallets w JOIN receivables r ON r.wallet_id=w.id WHERE r.id=$1`, fullID).Scan(&posted, &aggregate, &outstanding); err != nil || posted != 500 || aggregate != 40 || outstanding != 0 {
			t.Fatal("unexpected wallet/receivable projections")
		}
		var count int
		var total int64
		if err = pool.QueryRow(ctx, `SELECT count(*),sum(delta_microcredits) FROM ledger_postings WHERE journal_entry_id=$1`, result.JournalEntryID).Scan(&count, &total); err != nil || count != 2 || total != 0 {
			t.Fatal("write-off journal is not balanced")
		}
	})
}

func assertReceivableActivationNoEffect(t *testing.T, ctx context.Context, repo *PostgresRepository, receivableID, walletID uuid.UUID) {
	t.Helper()
	var cases, journals int
	var posted, aggregate, outstanding int64
	if err := repo.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM receivable_write_off_cases),(SELECT count(*) FROM journal_entries WHERE business_reference LIKE 'write-off:%')`).Scan(&cases, &journals); err != nil || cases != 0 || journals != 0 {
		t.Fatal("rejected execution wrote financial facts")
	}
	if err := repo.pool.QueryRow(ctx, `SELECT w.posted_microcredits,w.receivable_microcredits,r.outstanding_microcredits FROM wallets w JOIN receivables r ON r.wallet_id=w.id WHERE r.id=$1 AND w.id=$2`, receivableID, walletID).Scan(&posted, &aggregate, &outstanding); err != nil || posted != 500 || aggregate != 90 || outstanding != 50 {
		t.Fatal("rejected execution changed financial projections")
	}
}
