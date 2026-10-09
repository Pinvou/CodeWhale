package billing

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestLedgerRepairPostgresIntegration proves the GSA-24 slice-2 domain
// contract on real PostgreSQL: one equal-amount attribution reversal, a closed
// contract-based rejection for every other shape, the zero-change guarantee for
// wallet/lot/entitlement/receivable facts, and result-first idempotency.
func TestLedgerRepairPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	repository, schema := newAdminPaginationIntegrationRepository(t, ctx, databaseURL)
	pool := repository.pool
	_ = schema
	now := time.Now().UTC()

	walletID, walletAccountID := uuid.New(), uuid.New()
	ownerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO wallets(id,owner_type,owner_id,status,posted_microcredits,reserved_microcredits,version,created_at,updated_at)
VALUES($1,'PERSONAL',$2,'ACTIVE',100,0,3,$3,$3)`, walletID, ownerID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ledger_accounts(id,wallet_id,code,account_type,created_at) VALUES($1,$2,$3,'WALLET_LIABILITY',$4)`,
		walletAccountID, walletID, "wallet:"+walletID.String(), now); err != nil {
		t.Fatal(err)
	}
	targetJournalID, wrongPostingID := uuid.New(), uuid.New()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO journal_entries(id,entry_type,business_reference,description,created_at) VALUES($1,'USAGE',$2,'attribution error',$3)`,
		targetJournalID, "usage:"+targetJournalID.String(), now); err != nil {
		t.Fatal(err)
	}
	// The revenue leg was attributed to the adjustment account.
	if _, err := tx.Exec(ctx, `INSERT INTO ledger_postings(id,journal_entry_id,ledger_account_id,delta_microcredits,created_at) VALUES($1,$2,$3,30,$4)`,
		wrongPostingID, targetJournalID, adjustmentAccount, now); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger_postings(id,journal_entry_id,ledger_account_id,delta_microcredits,created_at) VALUES($1,$2,$3,-30,$4)`,
		uuid.New(), targetJournalID, walletAccountID, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	command := LedgerRepairCommandInput{
		TargetJournalEntryID: targetJournalID, TargetPostingID: wrongPostingID,
		WrongLedgerAccountID: adjustmentAccount, CorrectLedgerAccountID: usageRevenueAccount, AmountMicrocredits: 30,
	}
	plan, err := repository.LedgerRepairPlanForCommand(ctx, command, 1, 1, 1)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.EntryType != "USAGE" || plan.WrongLedgerAccountType != "ADJUSTMENT" || plan.CorrectLedgerAccountType != "USAGE_REVENUE" ||
		plan.AmountMicrocredits != 30 || plan.PostingSetDigest == "" || plan.FactsDigest == "" {
		t.Fatalf("unexpected plan %+v", plan)
	}
	if _, err := repository.LedgerRepairPlanForCommand(ctx, LedgerRepairCommandInput{
		TargetJournalEntryID: targetJournalID, TargetPostingID: wrongPostingID,
		WrongLedgerAccountID: adjustmentAccount, CorrectLedgerAccountID: adjustmentAccount, AmountMicrocredits: 30,
	}, 1, 1, 1); err == nil {
		t.Fatal("plan accepted identical wrong/correct accounts")
	}
	if _, err := repository.LedgerRepairPlanForCommand(ctx, LedgerRepairCommandInput{
		TargetJournalEntryID: targetJournalID, TargetPostingID: wrongPostingID,
		WrongLedgerAccountID: adjustmentAccount, CorrectLedgerAccountID: usageRevenueAccount, AmountMicrocredits: 31,
	}, 1, 1, 1); err == nil {
		t.Fatal("plan accepted a mismatched amount")
	}
	if _, err := repository.LedgerRepairPlanForCommand(ctx, LedgerRepairCommandInput{
		TargetJournalEntryID: targetJournalID, TargetPostingID: uuid.New(),
		WrongLedgerAccountID: adjustmentAccount, CorrectLedgerAccountID: usageRevenueAccount, AmountMicrocredits: 30,
	}, 1, 1, 1); err == nil {
		t.Fatal("plan accepted an unknown target posting")
	}
	// A wallet-account posting is out of first-release scope.
	if _, err := repository.LedgerRepairPlanForCommand(ctx, LedgerRepairCommandInput{
		TargetJournalEntryID: targetJournalID, TargetPostingID: wrongPostingID,
		WrongLedgerAccountID: walletAccountID, CorrectLedgerAccountID: usageRevenueAccount, AmountMicrocredits: 30,
	}, 1, 1, 1); err == nil {
		t.Fatal("plan accepted a wallet-account target")
	}

	before := ledgerRepairZeroChangeSnapshot(t, ctx, pool)
	execution := LedgerRepairExecutionInput{
		ApprovalRequestID: uuid.New(), AuthorizationEventID: uuid.New(), ApprovalEpoch: 4,
		ExecutionKey: "ledger-repair-" + targetJournalID.String(), PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: 1,
		Command: command,
		Frozen: LedgerRepairFrozenFacts{
			TargetJournalEntryID: targetJournalID, TargetPostingID: wrongPostingID,
			WrongLedgerAccountID: adjustmentAccount, CorrectLedgerAccountID: usageRevenueAccount,
			EntryType: plan.EntryType, AmountMicrocredits: plan.AmountMicrocredits,
			PostingSetDigest: plan.PostingSetDigest, FactsDigest: plan.FactsDigest,
		},
	}
	result, err := repository.ApplyLedgerRepair(ctx, execution)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if result.Status != "SUCCEEDED" || result.EntryType != "USAGE" || result.AmountMicrocredits != 30 || result.JournalEntryID == uuid.Nil {
		t.Fatalf("unexpected result %+v", result)
	}
	var entryType, reference string
	if err := pool.QueryRow(ctx, `SELECT entry_type,business_reference FROM journal_entries WHERE id=$1`, result.JournalEntryID).
		Scan(&entryType, &reference); err != nil || entryType != "REVERSAL" || reference != "ledger-repair:"+wrongPostingID.String() {
		t.Fatalf("repair journal type=%s reference=%s err=%v", entryType, reference, err)
	}
	var postingCount int
	var postingSum int64
	if err := pool.QueryRow(ctx, `SELECT count(*),COALESCE(sum(delta_microcredits),0) FROM ledger_postings WHERE journal_entry_id=$1`, result.JournalEntryID).
		Scan(&postingCount, &postingSum); err != nil || postingCount != 2 || postingSum != 0 {
		t.Fatalf("repair postings count=%d sum=%d err=%v", postingCount, postingSum, err)
	}
	var lotCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ledger_postings WHERE journal_entry_id=$1 AND credit_lot_id IS NOT NULL`, result.JournalEntryID).Scan(&lotCount); err != nil || lotCount != 0 {
		t.Fatalf("repair postings carry credit lots: %d err=%v", lotCount, err)
	}
	ledgerRepairAssertZeroChange(t, ctx, pool, before)
	if _, err := repository.ApplyLedgerRepair(ctx, execution); err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	ledgerRepairAssertZeroChange(t, ctx, pool, before)
	var journalCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE business_reference=$1`, "ledger-repair:"+wrongPostingID.String()).Scan(&journalCount); err != nil || journalCount != 1 {
		t.Fatalf("repair journal count=%d err=%v", journalCount, err)
	}
	// A different approval must not create a second correction.
	other := execution
	other.ApprovalRequestID = uuid.New()
	other.AuthorizationEventID = uuid.New()
	if _, err := repository.ApplyLedgerRepair(ctx, other); err == nil {
		t.Fatal("second approval created a second correction")
	}
	// Once corrected, the same posting can no longer be planned again.
	if _, err := repository.LedgerRepairPlanForCommand(ctx, command, 1, 1, 1); err == nil {
		t.Fatal("plan accepted an already corrected target posting")
	}
	stored, err := repository.LedgerRepairForRequest(ctx, execution.ApprovalRequestID)
	if err != nil || stored != result {
		t.Fatalf("result-first read stored=%+v err=%v", stored, err)
	}
}

// TestLedgerRepairPlatformOnlyShapePostgresIntegration proves the frozen §5
// shape set on real PostgreSQL: a CHARGEBACK journal whose legs are the two
// platform accounts only — the GSA-31 compensation shape — is a legal shape, so
// an attribution error inside it can be repaired even though the journal has no
// wallet leg. The pre-fix judgment treated `WALLET_LIABILITY` as a mandatory
// leg and refused this shape outright (see TestLedgerRepairContractShapeSet...).
func TestLedgerRepairPlatformOnlyShapePostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	repository, _ := newAdminPaginationIntegrationRepository(t, ctx, databaseURL)
	pool := repository.pool
	now := time.Now().UTC()

	// The chargeback's receivable leg was attributed to the adjustment account:
	// neither the current signature (ADJUSTMENT is not a CHARGEBACK account
	// type) nor the repaired one (two platform legs, no wallet leg) may be
	// refused.
	targetJournalID, wrongPostingID := uuid.New(), uuid.New()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO journal_entries(id,entry_type,business_reference,description,created_at) VALUES($1,'CHARGEBACK',$2,'attribution error',$3)`,
		targetJournalID, "chargeback:"+targetJournalID.String(), now); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger_postings(id,journal_entry_id,ledger_account_id,delta_microcredits,created_at) VALUES($1,$2,$3,-30,$4)`,
		wrongPostingID, targetJournalID, adjustmentAccount, now); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger_postings(id,journal_entry_id,ledger_account_id,delta_microcredits,created_at) VALUES($1,$2,$3,30,$4)`,
		uuid.New(), targetJournalID, chargebackClearingAccount, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	command := LedgerRepairCommandInput{
		TargetJournalEntryID: targetJournalID, TargetPostingID: wrongPostingID,
		WrongLedgerAccountID: adjustmentAccount, CorrectLedgerAccountID: receivableAccount, AmountMicrocredits: 30,
	}
	plan, err := repository.LedgerRepairPlanForCommand(ctx, command, 1, 1, 1)
	if err != nil {
		t.Fatalf("platform-only shape was refused: %v", err)
	}
	if plan.EntryType != "CHARGEBACK" || plan.WrongLedgerAccountType != "ADJUSTMENT" ||
		plan.CorrectLedgerAccountType != "RECEIVABLE_ASSET" || plan.AmountMicrocredits != 30 {
		t.Fatalf("unexpected plan %+v", plan)
	}
	before := ledgerRepairZeroChangeSnapshot(t, ctx, pool)
	result, err := repository.ApplyLedgerRepair(ctx, LedgerRepairExecutionInput{
		ApprovalRequestID: uuid.New(), AuthorizationEventID: uuid.New(), ApprovalEpoch: 5,
		ExecutionKey: "ledger-repair-" + targetJournalID.String(), PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: 1,
		Command: command,
		Frozen: LedgerRepairFrozenFacts{
			TargetJournalEntryID: targetJournalID, TargetPostingID: wrongPostingID,
			WrongLedgerAccountID: adjustmentAccount, CorrectLedgerAccountID: receivableAccount,
			EntryType: plan.EntryType, AmountMicrocredits: plan.AmountMicrocredits,
			PostingSetDigest: plan.PostingSetDigest, FactsDigest: plan.FactsDigest,
		},
	})
	if err != nil {
		t.Fatalf("apply platform-only shape: %v", err)
	}
	if result.Status != "SUCCEEDED" || result.EntryType != "CHARGEBACK" || result.AmountMicrocredits != 30 ||
		result.WrongLedgerAccountID != adjustmentAccount || result.CorrectLedgerAccountID != receivableAccount {
		t.Fatalf("unexpected result %+v", result)
	}
	var postingCount int
	var postingSum int64
	if err := pool.QueryRow(ctx, `SELECT count(*),COALESCE(sum(delta_microcredits),0) FROM ledger_postings WHERE journal_entry_id=$1`, result.JournalEntryID).
		Scan(&postingCount, &postingSum); err != nil || postingCount != 2 || postingSum != 0 {
		t.Fatalf("repair postings count=%d sum=%d err=%v", postingCount, postingSum, err)
	}
	// The target journal stays byte-identical and the repair touches no wallet,
	// lot, entitlement or receivable fact.
	confirmed, err := ledgerRepairReadTarget(ctx, pool, targetJournalID)
	if err != nil {
		t.Fatal(err)
	}
	if ledgerRepairPostingSetDigest(confirmed) != plan.PostingSetDigest {
		t.Fatal("target journal was rewritten")
	}
	ledgerRepairAssertZeroChange(t, ctx, pool, before)
}

type ledgerRepairZeroChange struct {
	WalletCount        int64
	WalletPostedSum    int64
	WalletVersionSum   int64
	LotCount           int64
	LotRemaining       int64
	ComponentAvailable int64
	ComponentUsed      int64
	PackagePointsUsed  int64
	ReceivableSettled  int64
	ReceivableOutstand int64
}

func ledgerRepairZeroChangeSnapshot(t *testing.T, ctx context.Context, pool *pgxpool.Pool) ledgerRepairZeroChange {
	t.Helper()
	var snapshot ledgerRepairZeroChange
	if err := pool.QueryRow(ctx, `SELECT count(*),COALESCE(sum(posted_microcredits),0),COALESCE(sum(version),0) FROM wallets`).
		Scan(&snapshot.WalletCount, &snapshot.WalletPostedSum, &snapshot.WalletVersionSum); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*),COALESCE(sum(remaining_microcredits),0) FROM credit_lots`).
		Scan(&snapshot.LotCount, &snapshot.LotRemaining); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(available_microcredits),0),COALESCE(sum(used_microcredits),0) FROM entitlement_components`).
		Scan(&snapshot.ComponentAvailable, &snapshot.ComponentUsed); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(points_used),0) FROM entitlement_packages`).Scan(&snapshot.PackagePointsUsed); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(settled_microcredits),0),COALESCE(sum(outstanding_microcredits),0) FROM receivables`).
		Scan(&snapshot.ReceivableSettled, &snapshot.ReceivableOutstand); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func ledgerRepairAssertZeroChange(t *testing.T, ctx context.Context, pool *pgxpool.Pool, before ledgerRepairZeroChange) {
	t.Helper()
	after := ledgerRepairZeroChangeSnapshot(t, ctx, pool)
	if after.WalletCount != before.WalletCount || after.WalletPostedSum != before.WalletPostedSum || after.WalletVersionSum != before.WalletVersionSum {
		t.Fatalf("wallet facts changed: before=%+v after=%+v", before, after)
	}
	if after.LotCount != before.LotCount || after.LotRemaining != before.LotRemaining {
		t.Fatalf("credit lot facts changed: before=%+v after=%+v", before, after)
	}
	if after.ComponentAvailable != before.ComponentAvailable || after.ComponentUsed != before.ComponentUsed ||
		after.PackagePointsUsed != before.PackagePointsUsed {
		t.Fatalf("entitlement facts changed: before=%+v after=%+v", before, after)
	}
	if after.ReceivableSettled != before.ReceivableSettled || after.ReceivableOutstand != before.ReceivableOutstand {
		t.Fatalf("receivable facts changed: before=%+v after=%+v", before, after)
	}
}
