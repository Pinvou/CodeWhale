package billing

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestLedgerRepairCaseImmutableAuditPostgresIntegration covers the GSA-24
// slice-1 contract of migration 000061: the immutable ledger repair case table
// exists with the designed closed value set, only minimal privileges are
// granted to the runtime role, and every mutation path (UPDATE/DELETE/TRUNCATE)
// is refused with the forward-only 55000 signal.
func TestLedgerRepairCaseImmutableAuditPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	repository, schema := newAdminPaginationIntegrationRepository(t, ctx, databaseURL)
	pool := repository.pool
	qualified := pgx.Identifier{schema}.Sanitize() + ".ledger_repair_cases"

	var canSelect, canInsert, canUpdate, canDelete bool
	if err := pool.QueryRow(ctx, `SELECT has_table_privilege('billing_service',$1,'SELECT'),has_table_privilege('billing_service',$1,'INSERT'),
has_table_privilege('billing_service',$1,'UPDATE'),has_table_privilege('billing_service',$1,'DELETE')`, qualified).
		Scan(&canSelect, &canInsert, &canUpdate, &canDelete); err != nil {
		t.Fatal(err)
	}
	if !canSelect || !canInsert || canUpdate || canDelete {
		t.Fatalf("ledger repair case privileges select=%v insert=%v update=%v delete=%v", canSelect, canInsert, canUpdate, canDelete)
	}

	now := time.Now().UTC()
	targetJournalID := uuid.Must(uuid.NewV7())
	targetPostingID := uuid.Must(uuid.NewV7())
	repairJournalID := uuid.Must(uuid.NewV7())
	// The balanced-journal trigger is DEFERRABLE INITIALLY DEFERRED, so both
	// postings of an entry must be written in one transaction.
	insertBalancedJournal := func(entryType, reference, description string, journalID, debitPostingID uuid.UUID, debitAccount, creditAccount uuid.UUID) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO journal_entries(id,entry_type,business_reference,description,created_at) VALUES($1,$2,$3,$4,$5)`,
			journalID, entryType, reference, description, now); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ledger_postings(id,journal_entry_id,ledger_account_id,delta_microcredits,created_at) VALUES($1,$2,$3,25,$4)`,
			debitPostingID, journalID, debitAccount, now); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ledger_postings(id,journal_entry_id,ledger_account_id,delta_microcredits,created_at) VALUES($1,$2,$3,-25,$4)`,
			uuid.Must(uuid.NewV7()), journalID, creditAccount, now); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	insertBalancedJournal("USAGE", "usage:"+targetJournalID.String(), "000061 target", targetJournalID, targetPostingID, adjustmentAccount, paymentClearingAccount)
	insertBalancedJournal("REVERSAL", "ledger-repair:"+targetPostingID.String(), "000061 repair", repairJournalID, uuid.Must(uuid.NewV7()), usageRevenueAccount, adjustmentAccount)
	digest := strings.Repeat("a", 64)
	insertCase := func(id uuid.UUID, posting uuid.UUID) error {
		_, err := pool.Exec(ctx, `INSERT INTO ledger_repair_cases(id,approval_request_id,authorization_event_id,approval_epoch,execution_key_digest,
target_journal_entry_id,target_posting_id,wrong_ledger_account_id,correct_ledger_account_id,entry_type,amount_microcredits,
posting_set_digest,facts_digest,journal_entry_id,state,created_at)
VALUES($1,$2,$3,1,$4,$5,$6,$7,$8,'USAGE',25,$9,$9,$10,'EXECUTED',$11)`,
			id, uuid.New(), uuid.New(), digest, targetJournalID, posting, adjustmentAccount, usageRevenueAccount, strings.Repeat("b", 64), repairJournalID, now)
		return err
	}
	caseID := uuid.Must(uuid.NewV7())
	if err := insertCase(caseID, targetPostingID); err != nil {
		t.Fatalf("insert ledger repair case: %v", err)
	}
	var storedEntryType, storedState string
	var storedAmount int64
	if err := pool.QueryRow(ctx, `SELECT entry_type,state,amount_microcredits FROM ledger_repair_cases WHERE id=$1`, caseID).
		Scan(&storedEntryType, &storedState, &storedAmount); err != nil || storedEntryType != "USAGE" || storedState != "EXECUTED" || storedAmount != 25 {
		t.Fatalf("stored case type=%s state=%s amount=%d err=%v", storedEntryType, storedState, storedAmount, err)
	}

	if _, err := pool.Exec(ctx, `UPDATE ledger_repair_cases SET amount_microcredits=26 WHERE id=$1`, caseID); err == nil {
		t.Fatal("immutable ledger repair case accepted UPDATE")
	} else if !strings.Contains(err.Error(), "ledger repair case is immutable") {
		t.Fatalf("unexpected UPDATE error: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM ledger_repair_cases WHERE id=$1`, caseID); err == nil {
		t.Fatal("immutable ledger repair case accepted DELETE")
	} else if !strings.Contains(err.Error(), "ledger repair case is immutable") {
		t.Fatalf("unexpected DELETE error: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE ledger_repair_cases`); err == nil {
		t.Fatal("immutable ledger repair case accepted TRUNCATE")
	} else if !strings.Contains(err.Error(), "ledger repair case is immutable") {
		t.Fatalf("unexpected TRUNCATE error: %v", err)
	}
	if err := insertCase(uuid.Must(uuid.NewV7()), targetPostingID); err == nil {
		t.Fatal("second case for the same target posting was accepted")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ledger_repair_cases(id,approval_request_id,authorization_event_id,approval_epoch,execution_key_digest,
target_journal_entry_id,target_posting_id,wrong_ledger_account_id,correct_ledger_account_id,entry_type,amount_microcredits,
posting_set_digest,facts_digest,journal_entry_id,state,created_at)
VALUES($1,$2,$3,1,$4,$5,$6,$7,$7,'USAGE',0,$8,$8,$9,'PENDING',$10)`,
		uuid.Must(uuid.NewV7()), uuid.New(), uuid.New(), digest, targetJournalID, uuid.Must(uuid.NewV7()), adjustmentAccount, strings.Repeat("c", 64), repairJournalID, now); err == nil {
		t.Fatal("case accepted zero amount, identical accounts and a non-EXECUTED state")
	}
}
