package billing

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The default INACTIVE activation must reject a valid fresh repair without any financial effect.
func TestLedgerRepairFreshInactiveHasNoFinancialEffect(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	repository, schema := newAdminPaginationIntegrationRepository(t, ctx, databaseURL)
	pool := repository.pool
	_ = schema
	verifier, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal("runtime verifier connection failed")
	}
	defer verifier.Close(ctx)
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrations.VerifyApplied(ctx, verifier, loaded); err != nil {
		t.Fatal("full installed migration verification failed", err)
	}
	var actor string
	var isSuper bool
	if err := verifier.QueryRow(ctx, `SELECT session_user,rolsuper FROM pg_roles WHERE rolname=session_user`).Scan(&actor, &isSuper); err != nil || actor != "billing_service" || isSuper {
		t.Fatal("actual runtime identity guard failed")
	}
	var activationState string
	var policy int64
	var version int64
	var schemaVersion int
	if err := pool.QueryRow(ctx, `SELECT state,policy_version,snapshot_schema_version,version FROM billing_approval_command_activations WHERE command_type='LEDGER_REPAIR'`).Scan(&activationState, &policy, &schemaVersion, &version); err != nil || activationState != "INACTIVE" || policy != 1 || schemaVersion != 1 || version != 1 {
		t.Fatal("default activation facts mismatch")
	}
	t.Log("full official Billing Up/Verify: real non-super billing_service; LEDGER_REPAIR default INACTIVE/v1/s1/version1")
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
	_, err = repository.ApplyLedgerRepair(ctx, execution)
	var freshCases, freshJournals, freshPostings int
	if queryErr := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM ledger_repair_cases WHERE approval_request_id=$1), (SELECT count(*) FROM journal_entries WHERE business_reference=$2), (SELECT count(*) FROM ledger_postings p JOIN journal_entries j ON j.id=p.journal_entry_id WHERE j.business_reference=$2)`, execution.ApprovalRequestID, "ledger-repair:"+wrongPostingID.String()).Scan(&freshCases, &freshJournals, &freshPostings); queryErr != nil {
		t.Fatal("fresh effect observation failed")
	}
	ledgerRepairAssertZeroChange(t, ctx, pool, before)
	if err == nil || freshCases != 0 || freshJournals != 0 || freshPostings != 0 {
		t.Fatalf("INACTIVE fresh domain wrote effect: success=%t cases=%d journals=%d postings=%d", err == nil, freshCases, freshJournals, freshPostings)
	}
	operator := ledgerActivationFixtureOperator(t, ctx, pool)
	execution.ActivationVersion = setLedgerActivationFixtureState(t, ctx, operator, "ACTIVE")
	plan, err = repository.LedgerRepairPlanForCommand(ctx, command, 1, 1, execution.ActivationVersion)
	if err != nil {
		t.Fatal("explicitly active frozen plan failed")
	}
	execution.Frozen.PostingSetDigest, execution.Frozen.FactsDigest = plan.PostingSetDigest, plan.FactsDigest
	assertNoEffect := func(t *testing.T) {
		t.Helper()
		if _, err := repository.ApplyLedgerRepair(ctx, execution); err == nil {
			t.Fatal("obsolete activation accepted a fresh financial write")
		}
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM ledger_repair_cases WHERE approval_request_id=$1), (SELECT count(*) FROM journal_entries WHERE business_reference=$2), (SELECT count(*) FROM ledger_postings p JOIN journal_entries j ON j.id=p.journal_entry_id WHERE j.business_reference=$2)`, execution.ApprovalRequestID, "ledger-repair:"+wrongPostingID.String()).Scan(&freshCases, &freshJournals, &freshPostings); err != nil {
			t.Fatal(err)
		}
		if freshCases != 0 || freshJournals != 0 || freshPostings != 0 {
			t.Fatal("rejected activation changed financial facts")
		}
		ledgerRepairAssertZeroChange(t, ctx, pool, before)
	}
	t.Run("operator-deactivated-after-plan", func(t *testing.T) {
		setLedgerActivationFixtureState(t, ctx, operator, "INACTIVE")
		assertNoEffect(t)
	})
	t.Run("new-activation-version-rejects-old-frozen-plan", func(t *testing.T) {
		setLedgerActivationFixtureState(t, ctx, operator, "ACTIVE")
		assertNoEffect(t)
	})
}
