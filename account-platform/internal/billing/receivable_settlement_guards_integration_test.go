package billing

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Real payment-query consumer, public migrations and billing_service writes.
// Provider facts and the exceptional storage faults are controlled in an owned
// UUID database; this is not live provider/channel acceptance.
func TestReceivableSettlementGuardedWritesPostgres(t *testing.T) {
	t.Run("recharge-and-write-off-lock-order", testReceivableWriteOffRechargeLockOrder)
	for _, scenario := range []string{"normal", "wallet-summary-drift", "receivable-update-suppressed", "payment-order-update-error"} {
		t.Run(scenario, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			c, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
			fact := c.provider.(*retainedFinancialProvider).result
			tx, err := r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			receivable, err := createReceivableWithinTransaction(ctx, tx, order.WalletID, "USAGE_OVERAGE", "guarded-settlement-"+uuid.NewString(), 10, &order.ID, nil, nil, nil, r.now())
			if err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "wallet-summary-drift":
				// Existing summary drift must not commit allocation/detail changes.
				if _, err := r.pool.Exec(ctx, `UPDATE wallets SET receivable_microcredits=0 WHERE id=$1`, order.WalletID); err != nil {
					t.Fatal(err)
				}
			case "receivable-update-suppressed":
				// Successful SQL with zero affected rows, installed by the fixture owner.
				if _, err := retainedFinanceOwnerExec(t, r, ctx, `CREATE FUNCTION test_suppress_receivable_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$; CREATE TRIGGER test_suppress_receivable_update BEFORE UPDATE ON receivables FOR EACH ROW EXECUTE FUNCTION test_suppress_receivable_update()`); err != nil {
					t.Fatal(err)
				}
			case "payment-order-update-error":
				if _, err := retainedFinanceOwnerExec(t, r, ctx, `CREATE FUNCTION test_fail_order_projection() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'controlled settlement projection failure' USING ERRCODE='P0001'; END $$; CREATE TRIGGER test_fail_order_projection BEFORE UPDATE ON payment_orders FOR EACH ROW EXECUTE FUNCTION test_fail_order_projection()`); err != nil {
					t.Fatal(err)
				}
			}
			before := settlementPaymentFactsSnapshot(t, ctx, r)
			result, err := c.applyQueriedPayment(ctx, order.ID, fact, false)
			if scenario != "normal" {
				// Check persisted facts before classification so the negative control
				// exposes a partial consumer commit rather than only an error mismatch.
				after := settlementPaymentFactsSnapshot(t, ctx, r)
				for table, original := range before {
					if original != after[table] {
						t.Errorf("rejected payment committed changes to %s", table)
					}
				}
				if scenario == "payment-order-update-error" {
					var databaseError *pgconn.PgError
					if !errors.As(err, &databaseError) || databaseError.Code != "P0001" {
						t.Errorf("database error was not propagated: %v", err)
					}
				} else if err == nil || errors.Is(err, ErrConflict) {
					t.Errorf("integrity failure must abort, not enter committable business conflict handling: %v", err)
				}
				if t.Failed() {
					return
				}
				switch scenario {
				case "wallet-summary-drift":
					if _, err := r.pool.Exec(ctx, `UPDATE wallets SET receivable_microcredits=10 WHERE id=$1`, order.WalletID); err != nil {
						t.Fatal(err)
					}
				case "receivable-update-suppressed":
					if _, err := retainedFinanceOwnerExec(t, r, ctx, `DROP TRIGGER test_suppress_receivable_update ON receivables; DROP FUNCTION test_suppress_receivable_update()`); err != nil {
						t.Fatal(err)
					}
				case "payment-order-update-error":
					if _, err := retainedFinanceOwnerExec(t, r, ctx, `DROP TRIGGER test_fail_order_projection ON payment_orders; DROP FUNCTION test_fail_order_projection()`); err != nil {
						t.Fatal(err)
					}
				}
				// The same provider transaction must remain fulfillable after recovery.
				result, err = c.applyQueriedPayment(ctx, order.ID, fact, false)
			}
			if err != nil || result.Status != "PAID" || result.FulfillmentStatus != "CREDITED" {
				t.Fatal("payment not fulfilled", result.Status, result.FulfillmentStatus, err)
			}
			var settled, outstanding, walletDebt, allocated int64
			var status string
			if err := r.pool.QueryRow(ctx, `SELECT settled_microcredits,outstanding_microcredits,status,(SELECT receivable_microcredits FROM wallets WHERE id=$2),(SELECT COALESCE(sum(amount_microcredits),0) FROM receivable_allocations WHERE receivable_id=$1) FROM receivables WHERE id=$1`, receivable.ID, order.WalletID).Scan(&settled, &outstanding, &status, &walletDebt, &allocated); err != nil || settled != 10 || outstanding != 0 || status != "SETTLED" || walletDebt != 0 || allocated != 10 {
				t.Fatal("settlement facts differ", settled, outstanding, status, walletDebt, allocated, err)
			}
			successful := settlementPaymentFactsSnapshot(t, ctx, r)
			// Duplicate observations legitimately refresh the order projection.
			delete(successful, "payment_orders")
			if _, err := c.applyQueriedPayment(ctx, order.ID, fact, false); err != nil {
				t.Fatal("duplicate retry", err)
			}
			duplicate := settlementPaymentFactsSnapshot(t, ctx, r)
			delete(duplicate, "payment_orders")
			if !reflect.DeepEqual(successful, duplicate) {
				t.Fatal("duplicate payment changed financial/evidence facts")
			}
		})
	}
}

func settlementPaymentFactsSnapshot(t *testing.T, ctx context.Context, r *PostgresRepository) map[string]string {
	t.Helper()
	facts := entitlementFinancialFactsSnapshot(t, ctx, r)
	for _, table := range []string{"payment_evidence", "payment_anomaly_cases"} {
		var rows string
		if err := r.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(row ORDER BY row::text),'[]'::jsonb)::text FROM (SELECT to_jsonb(f) AS row FROM `+pgx.Identifier{table}.Sanitize()+` f) facts`).Scan(&rows); err != nil {
			t.Fatalf("snapshot %s: %v", table, err)
		}
		facts[table] = rows
	}
	return facts
}
