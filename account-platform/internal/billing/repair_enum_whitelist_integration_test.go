package billing

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestRepairJournalAndReceivableEnumsPostgresIntegration proves migration
// 000060 restores the two enumerations the existing owner paths already emit:
// verified payment repair credit and reviewed provider forced refund journals,
// plus the BASE-shortfall receivable created by a confirmed forced refund.
// Every value below is owner-derived; the tables stay append-only.
func TestRepairJournalAndReceivableEnumsPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	repository, _ := newAdminPaginationIntegrationRepository(t, ctx, databaseURL)
	pool := repository.pool
	now := time.Now().UTC()

	for index, entryType := range []string{"REPAIR_CREDIT", "FORCED_REFUND", "EXPIRY"} {
		journalID, debitID, creditID := uuid.New(), uuid.New(), uuid.New()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO journal_entries(id,entry_type,business_reference,description,created_at) VALUES($1,$2,$3,'000060 fixture',$4)`,
			journalID, entryType, "repair-enum-"+entryType+"-"+journalID.String(), now); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("journal entry %s: %v", entryType, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ledger_postings(id,journal_entry_id,ledger_account_id,delta_microcredits,created_at) VALUES($1,$2,$3,10,$4)`,
			debitID, journalID, paymentClearingAccount, now); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("debit posting %s: %v", entryType, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ledger_postings(id,journal_entry_id,ledger_account_id,delta_microcredits,created_at) VALUES($1,$2,$3,-10,$4)`,
			creditID, journalID, adjustmentAccount, now); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("credit posting %s: %v", entryType, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit %s: %v", entryType, err)
		}
		_ = index
	}

	walletID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO wallets(id,owner_type,owner_id,status,created_at,updated_at) VALUES($1,'PERSONAL',$2,'ACTIVE',$3,$3)`,
		walletID, uuid.New(), now); err != nil {
		t.Fatal(err)
	}
	receivableID := uuid.New()
	reference := "forced-refund:" + receivableID.String()
	if _, err := pool.Exec(ctx, `INSERT INTO receivables(id,wallet_id,source_type,source_reference,original_microcredits,outstanding_microcredits,status,created_at,updated_at)
VALUES($1,$2,'FORCED_REFUND',$3,10,10,'OPEN',$4,$4)`, receivableID, walletID, reference, now); err != nil {
		t.Fatalf("forced refund receivable: %v", err)
	}
	var sourceType string
	if err := pool.QueryRow(ctx, `SELECT source_type FROM receivables WHERE id=$1`, receivableID).Scan(&sourceType); err != nil || sourceType != "FORCED_REFUND" {
		t.Fatalf("receivable projection=%s err=%v", sourceType, err)
	}
}
