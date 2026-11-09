package billingschema

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

func TestOrphanPaidOrderIndexPublicMigration(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("owned actual PostgreSQL schema fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	var predecessor []migrations.Migration
	var index migrations.Migration
	for _, m := range loaded {
		if m.Version == 81 {
			index = m
		} else {
			predecessor = append(predecessor, m)
		}
	}
	if index.Name != "orphan_payment_order_lookup" || index.UpSQL == "" || index.DownSQL == "" {
		t.Fatal("published index migration missing")
	}
	assertIndex := func(t *testing.T, c *pgx.Conn) uint32 {
		t.Helper()
		var oid uint32
		var keys []string
		var predicate string
		var valid bool
		err := c.QueryRow(ctx, `SELECT i.indexrelid,
   ARRAY(SELECT a.attname::text FROM unnest(i.indkey) WITH ORDINALITY k(attnum,n)
    JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.attnum ORDER BY k.n),
   pg_get_expr(i.indpred,i.indrelid),i.indisvalid AND i.indisready AND NOT i.indisunique
   AND i.indnkeyatts=4 AND i.indnatts=4 AND i.indexprs IS NULL
   AND pg_get_userbyid(c.relowner)='billing_schema_owner'
   AND NOT has_schema_privilege('billing_service','public','CREATE')
   AND NOT pg_has_role('billing_service','billing_schema_owner','MEMBER')
   FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid
   WHERE i.indexrelid=to_regclass('public.payment_orders_orphan_lookup_idx')
   AND i.indrelid='public.payment_orders'::regclass`).Scan(&oid, &keys, &predicate, &valid)
		if err != nil || !valid || !reflect.DeepEqual(keys, []string{"provider_id", "provider_version", "channel", "paid_at"}) || !strings.Contains(predicate, "status = 'PAID'") || !strings.Contains(predicate, "fulfillment_status = 'CREDITED'") {
			t.Fatalf("index catalog/authority differs: keys=%v valid=%v predicate=%s err=%v", keys, valid, predicate, err)
		}
		return oid
	}
	t.Run("fresh-failure-atomicity-and-retry", func(t *testing.T) {
		c := f.database(t, ctx, "billing", "billing_schema_owner")
		ordered, err := Ordered(loaded)
		if err != nil {
			t.Fatal(err)
		}
		injected := errors.New("controlled failure after index DDL")
		err = migrations.UpWithHooks(ctx, c, ordered, migrations.UpHooks{BeforeApply: prepareBillingMigration, ApplySQL: applyBillingMigration, LedgerRole: "billing_schema_owner", AfterApply: func(ctx context.Context, tx pgx.Tx, m migrations.Migration) error {
			if m.Version == 81 {
				return injected
			}
			return finishBillingMigration(ctx, tx, m)
		}})
		if !errors.Is(err, injected) {
			t.Fatal("injected boundary not reached", err)
		}
		var clean bool
		if err := c.QueryRow(ctx, `SELECT to_regclass('public.payment_orders_orphan_lookup_idx') IS NULL
   AND NOT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=81)
   AND NOT has_schema_privilege('billing_service','public','CREATE')`).Scan(&clean); err != nil || !clean {
			t.Fatal("failed migration leaked DDL, ledger or privilege", err)
		}
		if err := Up(ctx, c, loaded); err != nil {
			t.Fatal("retry public Up", err)
		}
		if _, err := migrations.VerifyApplied(ctx, c, loaded); err != nil {
			t.Fatal(err)
		}
		assertIndex(t, c)
	})
	t.Run("populated-upgrade-replay-and-own-ddl-rollback", func(t *testing.T) {
		c := f.database(t, ctx, "billing", "billing_schema_owner")
		if err := Up(ctx, c, predecessor); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, stageBillingSeed); err != nil {
			t.Fatal(err)
		}
		facts := func() string {
			return stageValue(t, ctx, c, `SELECT jsonb_build_object(
   'orders',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM payment_orders x),
   'transactions',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM payment_transactions x),
   'wallets',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM wallets x),
   'journals',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM journal_entries x),
   'postings',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM ledger_postings x),
   'history',(SELECT jsonb_agg(to_jsonb(x) ORDER BY version) FROM schema_migrations x WHERE version<>81))::text`)
		}
		// Controlled historical damage, not a normal payment producer: retain
		// complete original orders with both unknown and known payment dates.
		if _, err := c.Exec(ctx, `INSERT INTO payment_orders(
 id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,
 channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,
 provider_payable_until,late_review_boundary_at,final_archive_eligible_at,
 created_at,updated_at,provider_id,provider_version,paid_at)
 SELECT md5('orphan-upgrade-'||n)::uuid,'orphan-upgrade-'||n,
 '11111111-1111-4111-8111-111111111111'::uuid,'11111111-1111-4111-8111-111111111111'::uuid,
 '71111111-1111-4111-8111-111111111111'::uuid,'CNY_1',
 '{"product_id":"CNY_1","plan_name":"controlled legacy credit","amount_fen":100,"credit_microcredits":1000000,"currency":"CNY"}'::jsonb,
 'ALIPAY',100,'CNY',1000000,'PAID','CREDITED','2026-01-02'::timestamptz,
 '2026-01-02'::timestamptz,'2026-01-05'::timestamptz,'2026-01-10'::timestamptz,
 '2026-01-01'::timestamptz,'2026-01-01'::timestamptz,'controlled','v1',
 CASE WHEN n=1 THEN NULL ELSE '2026-01-01'::timestamptz END
 FROM generate_series(1,2) n`); err != nil {
			t.Fatal("seed retained original order evidence", err)
		}
		before := facts()
		if err := Up(ctx, c, loaded); err != nil {
			t.Fatal("public incremental upgrade", err)
		}
		if _, err := migrations.VerifyApplied(ctx, c, loaded); err != nil {
			t.Fatal(err)
		}
		oid := assertIndex(t, c)
		if err := Up(ctx, c, loaded); err != nil {
			t.Fatal(err)
		}
		if assertIndex(t, c) != oid || facts() != before {
			t.Fatal("upgrade/replay changed facts or rebuilt index")
		}
		// Whole-chain forward-only guards remain intact. Exercise this migration's
		// exact published DDL in a rolled-back transaction without altering history.
		tx, err := c.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, index.DownSQL); err != nil {
			t.Fatal(err)
		}
		var absent bool
		if err := tx.QueryRow(ctx, `SELECT to_regclass('public.payment_orders_orphan_lookup_idx') IS NULL`).Scan(&absent); err != nil || !absent {
			t.Fatal("own Down did not remove index", err)
		}
		if _, err := tx.Exec(ctx, index.UpSQL); err != nil {
			t.Fatal(err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		if assertIndex(t, c) != oid || facts() != before {
			t.Fatal("own DDL rollback changed retained facts")
		}
	})
}
