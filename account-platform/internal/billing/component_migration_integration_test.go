package billing

import (
	"context"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
)

// Apply the actual unpublished Up/Down files around data in the immutable A07
// schema. The ledger is seeded with the old contract's distinct usage/refund
// facts; no mock migration, disabled constraint, or rewritten old SQL is used.
func TestPostgresComponentMigrationPreservesConsumedLegacyAndReviewHold(t *testing.T) {
	ctx := context.Background()
	pool := testdb.Open(t, "billing")
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range loaded {
		if migration.Version <= 15 {
			continue
		}
		err = migrations.DownOne(ctx, conn.Conn(), loaded)
		migrations.Unlock(ctx, conn.Conn())
		if err != nil {
			t.Fatal(err)
		}
	}
	conn.Release()
	now := time.Now().UTC()
	repo := &PostgresRepository{pool: pool, now: func() time.Time { return now }}
	wallet, _, err := repo.EnsureWallet(ctx, "PERSONAL", uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	lotID, usageID, holdID := uuid.New(), uuid.New(), uuid.New()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,original_microcredits,remaining_microcredits,refundable,expires_at,created_at) VALUES($1,$2,'RECHARGE','historical-credit',1000,700,true,NULL,$3)`, lotID, wallet.ID, now); err != nil {
		t.Fatal(err)
	}
	account, err := walletLedgerAccount(ctx, tx, wallet.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range []struct {
		kind        string
		amount      int64
		counterpart uuid.UUID
	}{{"RECHARGE", 1000, paymentClearingAccount}, {"USAGE", -200, usageRevenueAccount}, {"REFUND", -100, refundClearingAccount}} {
		id := uuid.New()
		if err = insertJournal(ctx, tx, id, fact.kind, "migration:"+id.String(), "old financial fact", now); err != nil {
			t.Fatal(err)
		}
		if err = insertPosting(ctx, tx, id, account, fact.amount, &lotID, now); err != nil {
			t.Fatal(err)
		}
		if err = insertPosting(ctx, tx, id, fact.counterpart, -fact.amount, nil, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE wallets SET posted_microcredits=700,reserved_microcredits=300 WHERE id=$1`, wallet.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO usage_holds(id,usage_request_id,wallet_id,status,max_microcredits,expires_at,created_at,updated_at) VALUES($1,$2,$3,'REVIEW',300,$4,$5,$5)`, holdID, usageID, wallet.ID, now.Add(-time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	assertSnapshot := func() {
		t.Helper()
		var available, reserved, used, revoked, expired, total int64
		var expiry *time.Time
		if err := pool.QueryRow(ctx, `SELECT original_microcredits,available_microcredits,reserved_microcredits,used_microcredits,revoked_microcredits,expired_microcredits,expires_at FROM entitlement_components WHERE credit_lot_id=$1`, lotID).Scan(&total, &available, &reserved, &used, &revoked, &expired, &expiry); err != nil {
			t.Fatal(err)
		}
		if total != 1000 || available != 400 || reserved != 300 || used != 200 || revoked != 100 || expired != 0 || expiry != nil {
			t.Fatal("migration changed conserved historical buckets or invented expiry")
		}
		var backing int64
		if err := pool.QueryRow(ctx, `SELECT sum(reserved_microcredits) FROM usage_hold_allocations WHERE hold_id=$1`, holdID).Scan(&backing); err != nil || backing != 300 {
			t.Fatal("legacy REVIEW has no exact conserved allocation")
		}
		for name, count := range runLedgerRestoreAudit(t, repo) {
			if count != 0 {
				t.Fatalf("migration audit %s=%d", name, count)
			}
		}
		items, summary, err := repo.ListPackages(ctx, wallet.OwnerID, wallet.ID, 100)
		if err != nil || len(items) != 0 || summary.ActivePackageCount != 0 {
			t.Fatal("internal legacy accounting was added to personal PLAN list")
		}
	}
	apply := func(up bool) {
		t.Helper()
		c, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Release()
		if up {
			err = migrations.Up(ctx, c.Conn(), loaded)
		} else {
			for _, migration := range loaded {
				if migration.Version <= 15 {
					continue
				}
				err = migrations.DownOne(ctx, c.Conn(), loaded)
				migrations.Unlock(ctx, c.Conn())
				if err != nil {
					break
				}
			}
		}
		if up {
			migrations.Unlock(ctx, c.Conn())
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	apply(true)
	assertSnapshot()
	apply(false)
	var oldExpiry *time.Time
	if err = pool.QueryRow(ctx, `SELECT expires_at FROM credit_lots WHERE id=$1`, lotID).Scan(&oldExpiry); err != nil || oldExpiry != nil {
		t.Fatal("Down changed old no-expiry lot")
	}
	apply(true)
	assertSnapshot()
	now = now.Add(365 * 24 * time.Hour)
	if count, err := repo.ExpirePackages(ctx, 100); err != nil || count != 0 {
		t.Fatal("legacy funds gained an expiry")
	}
	hold, err := repo.Commit(ctx, usageID, holdID, 150, map[string]any{"input_tokens": 150})
	if err != nil || hold.Status != "COMMITTED" {
		t.Fatal("migrated REVIEW could not settle once with known facts")
	}
	if _, err = repo.Commit(ctx, usageID, holdID, 150, map[string]any{"input_tokens": 150}); err != nil {
		t.Fatal(err)
	}
	w, err := repo.GetWallet(ctx, wallet.ID)
	if err != nil || w.PostedMicrocredits != 550 || w.ReservedMicrocredits != 0 {
		t.Fatal("migrated settlement altered wallet conservation")
	}
	for name, count := range runLedgerRestoreAudit(t, repo) {
		if count != 0 {
			t.Fatalf("settled migration audit %s=%d", name, count)
		}
	}
}
