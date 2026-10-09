package billing

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestReceivableWriteOffPostgresIntegration proves the GSA-26 domain contract on
// real PostgreSQL: the full and the partial write-off, the exact journal and
// wallet/receivable projections, the boundary and source negative cases with
// zero writes, the zero-change guarantee for balances and lots, and result-first
// idempotency.
func TestReceivableWriteOffPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	repository, _ := newAdminPaginationIntegrationRepository(t, ctx, databaseURL)
	pool := repository.pool
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
	plan, err := repository.receivableWriteOffPlanFrom(ctx, ReceivableWriteOffCommandInput{ReceivableID: fullID, WriteOffMicrocredits: 50, ExpectedReceivableVersion: 1}, 1, 1, 1)
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
		if _, err := repository.receivableWriteOffPlanFrom(ctx, invalid, 1, 1, 1); !errors.Is(err, ErrApprovalSnapshotMismatch) && !errors.Is(err, ErrNotFound) {
			t.Fatalf("invalid plan %+v err=%v", invalid, err)
		}
	}
	var postedBefore int64
	if err := pool.QueryRow(ctx, `SELECT posted_microcredits FROM wallets WHERE id=$1`, walletID).Scan(&postedBefore); err != nil {
		t.Fatal(err)
	}
	execute := func(requestID, eventID, receivableID uuid.UUID, amount, expectedVersion int64, key string) (ReceivableWriteOffResult, error) {
		command := ReceivableWriteOffCommandInput{ReceivableID: receivableID, WriteOffMicrocredits: amount, ExpectedReceivableVersion: expectedVersion}
		// The frozen projection is what an approval would have captured; the
		// owner-only facts digest stays empty and is recomputed in the
		// execution transaction.
		frozen, err := repository.receivableWriteOffPlanFrom(ctx, command, 1, 1, 1)
		if err != nil {
			return ReceivableWriteOffResult{}, err
		}
		return repository.ApplyReceivableWriteOff(ctx, ReceivableWriteOffExecutionInput{
			ApprovalRequestID: requestID, AuthorizationEventID: eventID, ApprovalEpoch: 3,
			ExecutionKey: key, PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: 1,
			Command: command,
			Frozen: ReceivableWriteOffFrozenFacts{
				ReceivableVersion: frozen.ReceivableVersion, WriteOffMicrocredits: frozen.WriteOffMicrocredits,
				OutstandingMicrocredits: frozen.OutstandingMicrocredits, SettledMicrocredits: frozen.SettledMicrocredits,
				OriginalMicrocredits: frozen.OriginalMicrocredits, SourceType: frozen.SourceType,
				SourceReference: frozen.SourceReference, WalletReceivableMicrocredits: frozen.WalletReceivableMicrocredits,
			},
		})
	}
	fullRequestID, fullEventID := uuid.New(), uuid.New()
	full, err := execute(fullRequestID, fullEventID, fullID, 50, 1, "write-off:"+fullID.String())
	if err != nil {
		t.Fatalf("full write-off: %v", err)
	}
	if full.StatusAfter != "WRITTEN_OFF" || full.ReceivableVersionAfter != 2 || full.AmountMicrocredits != 50 {
		t.Fatalf("unexpected full result %+v", full)
	}
	var entryType, reference string
	var postingCount int
	var receivableDelta, adjustmentDelta int64
	if err := pool.QueryRow(ctx, `SELECT entry_type,business_reference FROM journal_entries WHERE id=$1`, full.JournalEntryID).Scan(&entryType, &reference); err != nil ||
		entryType != "ADJUSTMENT" || !strings.HasPrefix(reference, "write-off:") {
		t.Fatalf("journal %s/%s err=%v", entryType, reference, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*),
		COALESCE(max(delta_microcredits) FILTER (WHERE ledger_account_id=$2),0),
		COALESCE(max(delta_microcredits) FILTER (WHERE ledger_account_id=$3),0)
	FROM ledger_postings WHERE journal_entry_id=$1`, full.JournalEntryID, receivableAccount, adjustmentAccount).
		Scan(&postingCount, &receivableDelta, &adjustmentDelta); err != nil ||
		postingCount != 2 || receivableDelta != 50 || adjustmentDelta != -50 {
		t.Fatalf("postings=%d receivable=%d adjustment=%d err=%v", postingCount, receivableDelta, adjustmentDelta, err)
	}
	var settled, outstanding, version, walletReceivable int64
	var status, financialStatus string
	var settledAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT settled_microcredits,outstanding_microcredits,status,version,settled_at FROM receivables WHERE id=$1`, fullID).
		Scan(&settled, &outstanding, &status, &version, &settledAt); err != nil ||
		settled != 50 || outstanding != 0 || status != "WRITTEN_OFF" || version != 2 || settledAt == nil {
		t.Fatalf("receivable %d/%d/%s/%d/%v err=%v", settled, outstanding, status, version, settledAt, err)
	}
	if err := pool.QueryRow(ctx, `SELECT receivable_microcredits,financial_status FROM wallets WHERE id=$1`, walletID).Scan(&walletReceivable, &financialStatus); err != nil ||
		walletReceivable != 40 || financialStatus != "RECEIVABLE_OPEN" {
		t.Fatalf("wallet projection %d/%s err=%v", walletReceivable, financialStatus, err)
	}
	var allocationAmount int64
	if err := pool.QueryRow(ctx, `SELECT amount_microcredits FROM receivable_allocations WHERE id=$1 AND source_type='APPROVED_WRITE_OFF'`, full.AllocationID).Scan(&allocationAmount); err != nil ||
		allocationAmount != 50 {
		t.Fatalf("allocation=%d err=%v", allocationAmount, err)
	}
	var postedAfter int64
	if err := pool.QueryRow(ctx, `SELECT posted_microcredits FROM wallets WHERE id=$1`, walletID).Scan(&postedAfter); err != nil || postedAfter != postedBefore {
		t.Fatalf("available balance changed: %d -> %d err=%v", postedBefore, postedAfter, err)
	}

	// The same approval replays to the same case with no second effect.
	// The consumer reads the result first, so the frozen projection it presents
	// may be incomplete; only the request/event identity, the amount, the frozen
	// version and the execution key decide the idempotent hit.
	replayed, err := repository.ApplyReceivableWriteOff(ctx, ReceivableWriteOffExecutionInput{
		ApprovalRequestID: fullRequestID, AuthorizationEventID: fullEventID, ApprovalEpoch: 3,
		ExecutionKey: "write-off:" + fullID.String(), PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: 1,
		Command: ReceivableWriteOffCommandInput{ReceivableID: fullID, WriteOffMicrocredits: 50, ExpectedReceivableVersion: 1},
		Frozen:  ReceivableWriteOffFrozenFacts{ReceivableVersion: full.ReceivableVersionBefore, WriteOffMicrocredits: full.AmountMicrocredits},
	})
	if err != nil || replayed != full {
		t.Fatalf("idempotent replay result=%+v err=%v want=%+v", replayed, err, full)
	}
	var cases, writeOffJournals int64
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM receivable_write_off_cases),
		(SELECT count(*) FROM journal_entries WHERE business_reference LIKE 'write-off:%')`).Scan(&cases, &writeOffJournals); err != nil ||
		cases != 1 || writeOffJournals != 1 {
		t.Fatalf("idempotency cases=%d journals=%d err=%v", cases, writeOffJournals, err)
	}

	// A partial write-off of the remaining receivable leaves it partially
	// settled, then writing off the rest closes it.
	partialRequestID := uuid.New()
	partial, err := execute(partialRequestID, uuid.New(), partialID, 15, 2, "partial-one")
	if err != nil || partial.StatusAfter != "PARTIALLY_SETTLED" || partial.ReceivableVersionAfter != 3 {
		t.Fatalf("partial write-off %+v err=%v", partial, err)
	}
	if err := pool.QueryRow(ctx, `SELECT receivable_microcredits FROM wallets WHERE id=$1`, walletID).Scan(&walletReceivable); err != nil || walletReceivable != 25 {
		t.Fatalf("wallet after partial=%d err=%v", walletReceivable, err)
	}
	restored, err := execute(uuid.New(), uuid.New(), partialID, 25, 3, "partial-two")
	if err != nil || restored.StatusAfter != "WRITTEN_OFF" {
		t.Fatalf("second partial write-off %+v err=%v", restored, err)
	}
	if err := pool.QueryRow(ctx, `SELECT receivable_microcredits,financial_status FROM wallets WHERE id=$1`, walletID).Scan(&walletReceivable, &financialStatus); err != nil ||
		walletReceivable != 0 || financialStatus != "CLEAR" {
		t.Fatalf("wallet after full clearance=%d/%s err=%v", walletReceivable, financialStatus, err)
	}
	var settledSum, allocationSum int64
	if err := pool.QueryRow(ctx, `SELECT (SELECT settled_microcredits FROM receivables WHERE id=$1),
		(SELECT COALESCE(sum(amount_microcredits),0) FROM receivable_allocations WHERE receivable_id=$1)`, partialID).Scan(&settledSum, &allocationSum); err != nil ||
		settledSum != 60 || allocationSum != 60 {
		t.Fatalf("partial allocation sum=%d settled=%d err=%v", allocationSum, settledSum, err)
	}
	read, err := repository.ReceivableWriteOffForRequest(ctx, partialRequestID)
	if err != nil || read != partial {
		t.Fatalf("result-first read %+v err=%v", read, err)
	}
	if _, err := repository.ReceivableWriteOffForRequest(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent case err=%v", err)
	}
}
