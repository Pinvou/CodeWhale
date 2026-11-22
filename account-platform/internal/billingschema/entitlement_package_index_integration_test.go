package billingschema

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestF09EntitlementPackageIndexPublicMigration(t *testing.T) {
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
	var predecessor []migrations.Migration
	var index migrations.Migration
	for _, migration := range loaded {
		if migration.Version == 80 {
			index = migration
		} else {
			predecessor = append(predecessor, migration)
		}
	}
	if index.Name != "entitlement_components_package_index" || index.UpSQL == "" || index.DownSQL == "" {
		t.Fatal("the forward package index migration must be present")
	}
	assertIndex := func(t *testing.T, c *pgx.Conn) uint32 {
		t.Helper()
		var oid uint32
		var valid bool
		err := c.QueryRow(ctx, `SELECT i.indexrelid,
i.indisvalid AND i.indisready AND NOT i.indisunique AND NOT i.indisprimary
AND i.indnkeyatts=1 AND i.indnatts=1 AND i.indpred IS NULL AND i.indexprs IS NULL
AND am.amname='btree' AND a.attname='package_id'
AND pg_get_userbyid(idx.relowner)='billing_schema_owner'
FROM pg_index i JOIN pg_class idx ON idx.oid=i.indexrelid
JOIN pg_am am ON am.oid=idx.relam
JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=i.indkey[0]
WHERE i.indrelid='public.entitlement_components'::regclass
AND i.indexrelid=to_regclass('public.entitlement_components_package_idx')`).Scan(&oid, &valid)
		if err != nil || !valid {
			t.Fatalf("package index catalog shape or ownership differs: valid=%v err=%v", valid, err)
		}
		return oid
	}
	authority := func(c *pgx.Conn) string {
		return stageValue(t, ctx, c, `SELECT jsonb_build_array(current_user,session_user,
has_schema_privilege('billing_service','public','CREATE'),
pg_has_role('billing_service','billing_schema_owner','MEMBER'),
pg_get_userbyid(c.relowner),
has_table_privilege('billing_service',c.oid,'SELECT,INSERT,UPDATE,DELETE'))::text
FROM pg_class c WHERE c.oid='public.entitlement_components'::regclass`)
	}
	t.Run("fresh-public-up", func(t *testing.T) {
		c := f.database(t, ctx, "billing", "billing_schema_owner")
		ordered, err := Ordered(loaded)
		if err != nil {
			t.Fatal(err)
		}
		injected := errors.New("owned fixture after original080 DDL injection")
		err = migrations.UpWithHooks(ctx, c, ordered, migrations.UpHooks{
			BeforeApply: prepareBillingMigration, ApplySQL: applyBillingMigration, LedgerRole: "billing_schema_owner",
			AfterApply: func(ctx context.Context, tx pgx.Tx, m migrations.Migration) error {
				if m.Version != 80 {
					return finishBillingMigration(ctx, tx, m)
				}
				var loan bool
				if err := tx.QueryRow(ctx, `SELECT current_user='billing_service'
AND has_schema_privilege('billing_service','public','CREATE')
AND NOT pg_has_role('billing_service','billing_schema_owner','MEMBER')
AND COALESCE(current_setting('pinvou.package_index_ddl_loan',true),'')='true'`).Scan(&loan); err != nil || !loan {
					t.Fatalf("fresh080 must borrow only DDL CREATE: loan=%v err=%v", loan, err)
				}
				if err := finishBillingMigration(ctx, tx, m); err != nil {
					return err
				}
				var returned bool
				if err := tx.QueryRow(ctx, `SELECT current_user='billing_service' AND NOT has_schema_privilege('billing_service','public','CREATE')`).Scan(&returned); err != nil || !returned {
					t.Fatalf("fresh080 must return borrowed CREATE: returned=%v err=%v", returned, err)
				}
				return injected
			},
		})
		if err == nil || !strings.Contains(err.Error(), injected.Error()) {
			t.Fatal("080 rollback injection not reached", err)
		}
		if stageValue(t, ctx, c, `SELECT (to_regclass('public.entitlement_components_package_idx') IS NULL
AND NOT EXISTS(SELECT 1 FROM schema_migrations WHERE version=80)
AND NOT has_schema_privilege('billing_service','public','CREATE')
AND NOT pg_has_role('billing_service','billing_schema_owner','MEMBER'))::text`) != "true" {
			t.Fatal("failed080 index, ledger and privilege loan must roll back together")
		}
		// A pre-existing privilege is not our loan. Keep this entire controlled
		// probe in a rolled-back deployment transaction; public Up below still
		// starts with runtime CREATE revoked and exercises the actual fresh path.
		func() {
			tx, err := c.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, `SET ROLE billing_schema_owner; GRANT CREATE ON SCHEMA public TO billing_service; SET ROLE billing_service`); err != nil {
				t.Fatal(err)
			}
			if err := lendPackageIndexDDL(ctx, tx, index); err != nil {
				t.Fatal(err)
			}
			if err := revokePackageIndexDDL(ctx, tx, index); err != nil {
				t.Fatal(err)
			}
			var preserved bool
			if err := tx.QueryRow(ctx, `SELECT current_user='billing_service'
AND has_schema_privilege('billing_service','public','CREATE')
AND COALESCE(current_setting('pinvou.package_index_ddl_loan',true),'')<>'true'`).Scan(&preserved); err != nil || !preserved {
				t.Fatalf("an existing CREATE privilege must not be revoked: preserved=%v err=%v", preserved, err)
			}
		}()
		if err := Up(ctx, c, loaded); err != nil {
			t.Fatal("fresh public migration", err)
		}
		if _, err := migrations.VerifyApplied(ctx, c, loaded); err != nil {
			t.Fatal(err)
		}
		assertIndex(t, c)
		if authority(c) != `["billing_service", "billing_migrator", false, false, "billing_schema_owner", true]` {
			t.Fatal("fresh migration changed the original actor/ownership fence")
		}
	})
	t.Run("hardened-upgrade-preserves-multiple-components-and-facts", func(t *testing.T) {
		c := f.database(t, ctx, "billing", "billing_schema_owner")
		if err := Up(ctx, c, predecessor); err != nil {
			t.Fatal("complete hardened predecessor", err)
		}
		var absent bool
		if err := c.QueryRow(ctx, `SELECT to_regclass('public.entitlement_components_package_idx') IS NULL
AND NOT EXISTS(SELECT 1 FROM schema_migrations WHERE version=80)
AND EXISTS(SELECT 1 FROM billing_runtime_hardening)
AND NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='public.entitlement_components'::regclass
AND conname='entitlement_components_package_id_component_type_key')`).Scan(&absent); err != nil || !absent {
			t.Fatal("predecessor must lack the index after original058 and original hardening", err)
		}
		if _, err := c.Exec(ctx, stageBillingSeed); err != nil {
			t.Fatal(err)
		}
		walletID := uuid.MustParse("71111111-1111-4111-8111-111111111111")
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		var target uuid.UUID
		// Eight small packages make the equality/ANY probes selective without
		// turning this correctness fixture into a production capacity benchmark.
		for n := 0; n < 8; n++ {
			packageID, firstLot := uuid.New(), uuid.New()
			if n == 0 {
				target = packageID
			}
			if _, err := c.Exec(ctx, `INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,
original_microcredits,remaining_microcredits,refundable,expires_at,created_at)
VALUES($1,$2,'PROMOTION',$1::uuid::text,1,1,false,$3,$4)`, firstLot, walletID, now.Add(24*time.Hour), now); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Exec(ctx, `INSERT INTO entitlement_packages(id,wallet_id,credit_lot_id,
product_id,plan_name,sku_code,amount_fen,points_total,points_remaining,credited_at,valid_from,
valid_until,status,created_at,updated_at,source_type,source_reference)
VALUES($1,$2,$3,'owned-index','owned-index','',0,1,1,$4,$4,$5,'ACTIVE',$4,$4,'SYSTEM',$1::uuid::text)`, packageID, walletID, firstLot, now, now.Add(24*time.Hour)); err != nil {
				t.Fatal(err)
			}
			parts := 1
			if n == 0 {
				parts = 2
			}
			for part := 0; part < parts; part++ {
				lotID := firstLot
				if part > 0 {
					lotID = uuid.New()
					if _, err := c.Exec(ctx, `INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,
original_microcredits,remaining_microcredits,refundable,expires_at,created_at)
VALUES($1,$2,'PROMOTION',$1::uuid::text,1,1,false,$3,$4)`, lotID, walletID, now.Add(24*time.Hour), now); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := c.Exec(ctx, `INSERT INTO entitlement_components(id,package_id,component_type,
credit_lot_id,original_microcredits,available_microcredits,credited_at,expires_at,status,created_at,updated_at)
VALUES($1,$2,'BASE',$1,1,1,$3,$4,'ACTIVE',$3,$3)`, lotID, packageID, now, now.Add(24*time.Hour)); err != nil {
					t.Fatal("same-package two BASE components must remain legal", err)
				}
			}
		}
		facts := func() string {
			return stageValue(t, ctx, c, `SELECT jsonb_build_object(
'wallets',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM wallets t),
'lots',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM credit_lots t),
'packages',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM entitlement_packages t),
'components',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM entitlement_components t),
'journals',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM journal_entries t),
'postings',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM ledger_postings t),
'outbox',(SELECT jsonb_agg(to_jsonb(t) ORDER BY event_id) FROM outbox_events t))::text`)
		}
		oldLedger := func() string {
			return stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text
FROM schema_migrations m WHERE version<>80`)
		}
		readTarget := func() string {
			var rows string
			if err := c.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(e) ORDER BY id)::text
FROM entitlement_components e WHERE package_id=$1`, target).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := c.QueryRow(ctx, `SELECT count(*) FROM entitlement_components
WHERE package_id=ANY($1::uuid[]) AND component_type='BASE'`, []uuid.UUID{target}).Scan(&count); err != nil || count != 2 {
				t.Fatalf("package probe must retain both BASE rows exclusively: count=%d err=%v", count, err)
			}
			return rows
		}
		beforeFacts, beforeLedger, beforeAuthority, beforeRead := facts(), oldLedger(), authority(c), readTarget()
		if err := Up(ctx, c, loaded); err != nil {
			t.Fatal("ordinary incremental public Up", err)
		}
		if _, err := migrations.VerifyApplied(ctx, c, loaded); err != nil {
			t.Fatal(err)
		}
		oid := assertIndex(t, c)
		if err := Up(ctx, c, loaded); err != nil {
			t.Fatal("idempotent public Up", err)
		}
		if oid != assertIndex(t, c) || oldLedger() != beforeLedger || facts() != beforeFacts || authority(c) != beforeAuthority || readTarget() != beforeRead {
			t.Fatal("index installation/replay changed original migration, financial, authority or read facts")
		}
		var plan json.RawMessage
		if err := c.QueryRow(ctx, `EXPLAIN (FORMAT JSON) SELECT * FROM entitlement_components WHERE package_id=$1`, target).Scan(&plan); err != nil || !json.Valid(plan) {
			t.Fatal("actual controlled query plan", err)
		}
		t.Logf("controlled eight-package EXPLAIN (planner choice is not a capacity assertion): %s", plan)
		// Test only this new reversible DDL in its owned DB. The original
		// whole-chain forward-only DownOne guards and ledger remain untouched.
		tx, err := c.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE billing_schema_owner`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, index.DownSQL); err != nil {
			t.Fatal("own index DownSQL", err)
		}
		if err := tx.QueryRow(ctx, `SELECT to_regclass('public.entitlement_components_package_idx') IS NULL`).Scan(&absent); err != nil || !absent {
			t.Fatal("own DownSQL did not remove only its index", err)
		}
		if _, err := tx.Exec(ctx, index.UpSQL); err != nil {
			t.Fatal("own index UpSQL restoration", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		assertIndex(t, c)
		if _, err := migrations.VerifyApplied(ctx, c, loaded); err != nil {
			t.Fatal(err)
		}
		if facts() != beforeFacts || oldLedger() != beforeLedger || authority(c) != beforeAuthority || readTarget() != beforeRead {
			t.Fatal("own DDL rollback/restoration changed retained facts")
		}
	})
}
