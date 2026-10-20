package billingschema

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Exercise the real public upgrade and runtime readers; the seed is controlled
// database state, not a provider payment or approval execution acceptance test.
func TestF09ReaderFirstBeforeAndAfterCorrectionMigration(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("actual isolated PostgreSQL schema fixture is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	// This is the existing hardened predecessor selection: all original
	// migrations except the four correction phases and their dependent lock.
	var predecessor []migrations.Migration
	for _, m := range loaded {
		if (m.Version >= 56 && m.Version <= 59) || m.Version == 900013 {
			continue
		}
		predecessor = append(predecessor, m)
	}
	c := f.database(t, ctx, "billing", "billing_schema_owner")
	if err := Up(ctx, c, predecessor); err != nil {
		t.Fatal("public hardened predecessor", err)
	}
	var hardened, column bool
	if err := c.QueryRow(ctx, `SELECT to_regclass('public.billing_runtime_hardening') IS NOT NULL,
EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid='public.entitlement_components'::regclass AND attname='reclassified_out_microcredits' AND NOT attisdropped)`).Scan(&hardened, &column); err != nil || !hardened || column {
		t.Fatalf("real pre056 schema hardened=%v column=%v err=%v", hardened, column, err)
	}
	pool, err := pgxpool.New(ctx, f.serviceURL(t, c, "billing_service"))
	if err != nil {
		t.Fatal("runtime pool", err)
	}
	defer pool.Close()
	var session, current string
	var super bool
	if err := pool.QueryRow(ctx, `SELECT session_user,current_user,(SELECT rolsuper FROM pg_roles WHERE rolname=current_user)`).Scan(&session, &current, &super); err != nil || session != "billing_service" || current != "billing_service" || super {
		t.Fatal("reader must use the actual nonsuper runtime LOGIN", err)
	}
	repo := billing.NewPostgresRepositoryWithPool(pool)
	userID, walletID, orderID, transactionID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	packageID, lotID, targetLotID := uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	expires := now.Add(24 * time.Hour)
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE billing_schema_owner`); err != nil {
		t.Fatal(err)
	}
	for _, seed := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO wallets(id,owner_type,owner_id,status,posted_microcredits,created_at,updated_at) VALUES($1,'PERSONAL',$2,'ACTIVE',100,$3,$3)`, []any{walletID, userID, now}},
		{`INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at,created_at,updated_at,provider_id,provider_version) VALUES($1,$2,$3,$3,$4,'F09_READER_FIRST','{}','ALIPAY',100,'CNY',100,'PAID','CREDITED',$5,$5,$6,$7,$8,$8,'controlled-reader','v1')`, []any{orderID, "reader-first-" + orderID.String(), userID, walletID, now.Add(time.Hour), now.Add(72 * time.Hour), now.Add(8 * 24 * time.Hour), now}},
		{`INSERT INTO payment_transactions(id,payment_order_id,provider_id,provider_version,channel,provider_transaction_id,status,fulfillment_status,amount_fen,currency,source,provider_paid_at,verified_at,fulfilled_at,created_at,updated_at) VALUES($1,$2,'controlled-reader','v1','ALIPAY',$3,'PAID','CREDITED',100,'CNY','QUERY',$4,$4,$4,$4,$4)`, []any{transactionID, orderID, "reader-first-transaction:" + transactionID.String(), now}},
		{`INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,original_microcredits,remaining_microcredits,refundable,expires_at,created_at) VALUES($1,$2,'RECHARGE',$3,100,100,TRUE,$4,$5)`, []any{lotID, walletID, "reader-first:" + lotID.String(), expires, now}},
		{`INSERT INTO entitlement_packages(id,user_id,wallet_id,payment_order_id,source_payment_transaction_id,credit_lot_id,product_id,plan_name,sku_code,amount_fen,points_total,points_remaining,credited_at,valid_from,valid_until,status,created_at,updated_at,source_type,source_reference) VALUES($1,$2,$3,$4,$5,$6,'F09_READER_FIRST','Reader first','',100,100,100,$7,$7,$8,'ACTIVE',$7,$7,'PAYMENT',$9)`, []any{packageID, userID, walletID, orderID, transactionID, lotID, now, expires, "payment-transaction:" + transactionID.String()}},
		{`INSERT INTO entitlement_components(id,package_id,component_type,credit_lot_id,original_microcredits,available_microcredits,credited_at,expires_at,status,created_at,updated_at) VALUES($1,$2,'BASE',$1,100,100,$3,$4,'ACTIVE',$3,$3)`, []any{lotID, packageID, now, expires}},
	} {
		if _, err := tx.Exec(ctx, seed.sql, seed.args...); err != nil {
			t.Fatal("controlled reader fixture", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		t.Helper()
		var result string
		if err := c.QueryRow(ctx, `SELECT jsonb_build_object(
'wallet',(SELECT to_jsonb(w) FROM wallets w WHERE id=$1),
'order',(SELECT to_jsonb(o) FROM payment_orders o WHERE id=$2),
'payment_transactions',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM payment_transactions t WHERE payment_order_id=$2),
'package',(SELECT to_jsonb(p) FROM entitlement_packages p WHERE id=$3),
'components',(SELECT jsonb_agg(to_jsonb(e) ORDER BY id) FROM entitlement_components e WHERE package_id=$3),
'lots',(SELECT jsonb_agg(to_jsonb(l) ORDER BY id) FROM credit_lots l WHERE wallet_id=$1))::text`, walletID, orderID, packageID).Scan(&result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	assertComponents := func(t *testing.T, components []billing.EntitlementComponent, effective int64, count int) {
		t.Helper()
		if len(components) != count {
			t.Fatalf("components=%d want=%d", len(components), count)
		}
		for _, component := range components {
			if component.ID == lotID {
				if component.OriginalMicrocredits != 100 || component.EffectiveOriginalMicrocredits != effective {
					t.Fatalf("original/effective=%d/%d want=100/%d", component.OriginalMicrocredits, component.EffectiveOriginalMicrocredits, effective)
				}
				return
			}
		}
		t.Fatal("original source component missing")
	}
	readBoth := func(phase string, effective int64, count int) {
		t.Helper()
		t.Run(phase+"/personal-package-reader", func(t *testing.T) {
			before := snapshot()
			items, summary, err := repo.ListPackages(ctx, userID, walletID, 100)
			if err != nil || len(items) != 1 || items[0].ID != packageID || summary.AvailableMicrocredits != 100 {
				t.Fatalf("actual ListPackages items=%d summary=%+v err=%v", len(items), summary, err)
			}
			assertComponents(t, items[0].Components, effective, count)
			if before != snapshot() {
				t.Fatal("personal reader changed stored financial facts")
			}
		})
		t.Run(phase+"/admin-payment-reader", func(t *testing.T) {
			before := snapshot()
			item, err := repo.AdminPaymentOrder(ctx, orderID)
			if err != nil || item.ID != orderID || len(item.Packages) != 1 || item.Packages[0].ID != packageID {
				t.Fatalf("actual AdminPaymentOrder packages=%d err=%v", len(item.Packages), err)
			}
			assertComponents(t, item.Packages[0].Components, effective, count)
			if before != snapshot() {
				t.Fatal("admin reader changed stored financial facts")
			}
		})
	}
	readBoth("hardened-pre056-column-absent", 100, 1)
	if err := Up(ctx, c, loaded); err != nil {
		t.Fatal("ordinary complete public upgrade", err)
	}
	if _, err := migrations.VerifyApplied(ctx, c, loaded); err != nil {
		t.Fatal(err)
	}
	var installed int
	var owner, role string
	var read, insert, update, del bool
	if err := c.QueryRow(ctx, `SELECT pg_get_userbyid(relowner),has_table_privilege('billing_service',oid,'SELECT'),has_table_privilege('billing_service',oid,'INSERT'),has_table_privilege('billing_service',oid,'UPDATE'),has_table_privilege('billing_service',oid,'DELETE'),current_user,(SELECT count(*) FROM schema_migrations WHERE version BETWEEN 56 AND 59) FROM pg_class WHERE oid='public.entitlement_component_corrections'::regclass`).Scan(&owner, &read, &insert, &update, &del, &role, &installed); err != nil || owner != "billing_schema_owner" || !read || !insert || update || del || role != "billing_service" || installed != 4 {
		t.Fatalf("upgrade owner=%s R/I/U/D=%v/%v/%v/%v role=%s ledger=%d err=%v", owner, read, insert, update, del, role, installed, err)
	}
	// Keep a zero-column positive as well as the nonzero correction projection.
	readBoth("upgraded-zero-reclassification", 100, 1)
	updateTx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer updateTx.Rollback(context.Background())
	if _, err := updateTx.Exec(ctx, `SET LOCAL ROLE billing_schema_owner`); err != nil {
		t.Fatal(err)
	}
	// Controlled valid conservation state: transfer20 from BASE to BONUS. No
	// trigger or constraint is disabled, and no approval execution is claimed.
	if tag, err := updateTx.Exec(ctx, `UPDATE entitlement_components SET available_microcredits=80,reclassified_out_microcredits=20 WHERE id=$1`, lotID); err != nil || tag.RowsAffected() != 1 {
		t.Fatal("fixture effective source", err)
	}
	if tag, err := updateTx.Exec(ctx, `UPDATE credit_lots SET remaining_microcredits=80 WHERE id=$1`, lotID); err != nil || tag.RowsAffected() != 1 {
		t.Fatal("fixture source lot", err)
	}
	if _, err := updateTx.Exec(ctx, `INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,original_microcredits,remaining_microcredits,refundable,expires_at,created_at) VALUES($1,$2,'ADJUSTMENT',$3,20,20,FALSE,$4,$5)`, targetLotID, walletID, "reader-first:"+targetLotID.String(), expires, now); err != nil {
		t.Fatal(err)
	}
	if _, err := updateTx.Exec(ctx, `INSERT INTO entitlement_components(id,package_id,component_type,credit_lot_id,original_microcredits,available_microcredits,credited_at,expires_at,status,created_at,updated_at) VALUES($1,$2,'BONUS',$1,20,20,$3,$4,'ACTIVE',$3,$3)`, targetLotID, packageID, now, expires); err != nil {
		t.Fatal(err)
	}
	if err := updateTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	readBoth("upgraded-nonzero-reclassification", 80, 2)
	// Setup used the trusted migrator's existing owner membership, not a
	// privilege loan to the runtime LOGIN; ensure the original boundary stays.
	var runtimeOwner bool
	if err := pool.QueryRow(ctx, `SELECT pg_has_role('billing_service','billing_schema_owner','MEMBER')`).Scan(&runtimeOwner); err != nil || runtimeOwner {
		t.Fatal("runtime gained schema owner authority", err)
	}
}
