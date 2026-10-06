package testdb

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestBillingLedgerRunbookAgainstActualMigratedSchema(t *testing.T) {
	pool := Open(t, "billing")
	ctx := context.Background()
	audit := func(t *testing.T) map[string]int64 {
		t.Helper()
		root := filepath.Dir(filepath.Dir(migrationDirectory(t, "billing")))
		source, err := os.ReadFile(filepath.Join(root, "deploy", "runbooks", "sql", "billing-ledger-audit.sql"))
		if err != nil {
			t.Fatal(err)
		}
		// psql's ON_ERROR_STOP is a client directive; pgconn's ReadAll reports
		// every server error while executing the unchanged SQL transaction.
		sql := strings.TrimPrefix(strings.ReplaceAll(string(source), "\r\n", "\n"), "\\set ON_ERROR_STOP on\n")
		connection, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Release()
		results, err := connection.Conn().PgConn().Exec(ctx, sql).ReadAll()
		if err != nil {
			_, _ = connection.Exec(ctx, "ROLLBACK")
			t.Fatalf("execute actual audit runbook: %v", err)
		}
		checks := make(map[string]int64)
		for _, result := range results {
			if len(result.FieldDescriptions) == 0 {
				continue
			}
			if len(result.FieldDescriptions) != 2 || string(result.FieldDescriptions[0].Name) != "check_name" || string(result.FieldDescriptions[1].Name) != "violations" || len(result.Rows) != 1 || len(result.Rows[0]) != 2 {
				t.Fatal("audit result has an unexpected shape")
			}
			name := string(result.Rows[0][0])
			if _, duplicate := checks[name]; duplicate {
				t.Fatalf("duplicate audit check %s", name)
			}
			count, err := strconv.ParseInt(string(result.Rows[0][1]), 10, 64)
			if err != nil || count < 0 {
				t.Fatalf("invalid audit count for %s", name)
			}
			checks[name] = count
		}
		if len(checks) != 15 {
			t.Fatalf("audit executed %d checks; expected all 15", len(checks))
		}
		return checks
	}
	assertClean := func(t *testing.T, checks map[string]int64) {
		t.Helper()
		for name, count := range checks {
			if count != 0 {
				t.Errorf("legal fixture falsely reported %s: %d", name, count)
			}
		}
	}
	t.Run("fresh-migrated-schema", func(t *testing.T) {
		assertClean(t, audit(t))
	})
	var walletID uuid.UUID
	t.Run("written-off-is-a-legal-zero-outstanding-state", func(t *testing.T) {
		walletID = seedWrittenOffReceivableForAudit(t, pool)
		assertClean(t, audit(t))
	})
	t.Run("actual-wallet-receivable-snapshot-mismatch-is-detected", func(t *testing.T) {
		if walletID == uuid.Nil {
			t.Fatal("written-off fixture was not created")
		}
		if _, err := pool.Exec(ctx, `UPDATE wallets SET receivable_microcredits=1,financial_status='RECEIVABLE_OPEN' WHERE id=$1`, walletID); err != nil {
			t.Fatal(err)
		}
		checks := audit(t)
		if checks["wallet_receivable_snapshot_mismatch"] != 1 {
			t.Fatalf("real mismatch was not detected: %v", checks)
		}
		for name, count := range checks {
			if name != "wallet_receivable_snapshot_mismatch" && count != 0 {
				t.Errorf("unrelated check %s falsely reported %d", name, count)
			}
		}
	})
}

// This legal SQL fixture tests audit interpretation, not a production write-off
// command or approval flow. It uses only its own freshly migrated database.
func seedWrittenOffReceivableForAudit(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	walletID, receivableID, journalID := uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO wallets(id,owner_type,owner_id,status,created_at,updated_at) VALUES($1,'PERSONAL',$2,'ACTIVE',now(),now())`, walletID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO receivables(id,wallet_id,source_type,source_reference,original_microcredits,settled_microcredits,outstanding_microcredits,status,created_at,updated_at,settled_at) VALUES($1,$2,'USAGE_OVERAGE',$3,5,5,0,'WRITTEN_OFF',now(),now(),now())`, receivableID, walletID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO journal_entries(id,entry_type,business_reference,description,created_at) VALUES($1,'RECEIVABLE',$2,'controlled audit fixture',now())`, journalID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO receivable_allocations(id,receivable_id,wallet_id,source_type,source_reference,amount_microcredits,journal_entry_id,created_at) VALUES($1,$2,$3,'APPROVED_WRITE_OFF',$4,5,$5,now())`, uuid.New(), receivableID, walletID, uuid.NewString(), journalID); err != nil {
		t.Fatal(err)
	}
	return walletID
}
