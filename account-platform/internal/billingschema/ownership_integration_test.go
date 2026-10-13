package billingschema

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"os"
	"strings"
	"testing"
	"time"
)

func TestBillingOwnerLegacyAndIncrementalMigrations(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL")
	if raw == "" {
		t.Skip("isolated PostgreSQL integration requires PINVOU_TEST_BILLING_DATABASE_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	name := "billing_owner_review_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+quoted+" OWNER billing_schema_owner"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		clean, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		var owner string
		var active int
		for limit := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if e := admin.QueryRow(clean, `SELECT pg_get_userbyid(datdba),(SELECT count(*) FROM pg_stat_activity WHERE datname=$1) FROM pg_database WHERE datname=$1`, name).Scan(&owner, &active); e != nil {
				t.Error(e)
				return
			}
			if active == 0 || time.Now().After(limit) {
				break
			}
		}
		if owner != "billing_schema_owner" || active != 0 {
			t.Error("owned database cleanup identity guard")
			return
		}
		if _, e := admin.Exec(clean, "DROP DATABASE "+quoted); e != nil {
			t.Error(e)
		}
	}()
	config, err := pgx.ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	config.Database = name
	fixture, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.Close(context.Background())
	config.User = "billing_migrator"
	config.Password = ""
	c, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	if _, err = c.Exec(ctx, `SET ROLE billing_schema_owner; ALTER SCHEMA public OWNER TO billing_schema_owner; REVOKE CREATE ON SCHEMA public FROM PUBLIC; GRANT USAGE,CREATE ON SCHEMA public TO billing_service; SET ROLE billing_service`); err != nil {
		t.Fatal(err)
	}
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	var before []migrations.Migration
	for _, m := range loaded {
		if m.Version <= 28 || m.Version == generationVersion {
			before = append(before, m)
		}
	}
	if err = Up(ctx, c, before); err != nil {
		t.Fatal(err)
	}
	// Synthetic legacy row isolates the DDL compatibility boundary. FK validation
	// is disabled only in this superuser fixture transaction, never in migration.
	if _, err = fixture.Exec(ctx, `BEGIN; SET LOCAL session_replication_role=replica;
INSERT INTO chargeback_cases(id,payment_evidence_id,payment_order_id,payment_transaction_id,entitlement_package_id,wallet_id,provider_chargeback_id,status,revoked_base_microcredits,revoked_bonus_microcredits,created_at,resolved_at)
VALUES('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb','cccccccc-cccc-4ccc-8ccc-cccccccccccc','dddddddd-dddd-4ddd-8ddd-dddddddddddd','eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee','ffffffff-ffff-4fff-8fff-ffffffffffff','legacy-byte-evidence','RESOLVED',2,3,'2026-01-01','2026-01-02'); COMMIT;`); err != nil {
		t.Fatal(err)
	}
	var beforeEvidence string
	evidence := `SELECT jsonb_build_array(id,payment_evidence_id,payment_order_id,payment_transaction_id,entitlement_package_id,wallet_id,provider_chargeback_id,status,revoked_base_microcredits,revoked_bonus_microcredits,receivable_id,created_at,resolved_at)::text FROM chargeback_cases`
	if err = fixture.QueryRow(ctx, evidence).Scan(&beforeEvidence); err != nil {
		t.Fatal(err)
	}

	if _, err = fixture.Exec(ctx, `INSERT INTO payment_notifications(notification_id,channel,request_hash,payload,received_at,result) VALUES('66666666-6666-4666-8666-666666666666','FIXTURE','fixture-original-hash','{"immutable":"legacy-callback"}','2026-01-01','RECEIVED')`); err != nil {
		t.Fatal(err)
	}
	var oldNotification string
	notificationFacts := `SELECT jsonb_build_array(notification_id,channel,request_hash,payload,received_at,processed_at,result,payment_evidence_id)::text FROM payment_notifications`
	if err = fixture.QueryRow(ctx, notificationFacts).Scan(&oldNotification); err != nil {
		t.Fatal(err)
	}
	var withoutProvenance []migrations.Migration
	for _, m := range loaded {
		if m.Version != 900005 {
			withoutProvenance = append(withoutProvenance, m)
		}
	}
	if err = Up(ctx, c, withoutProvenance); err != nil {
		t.Fatal(err)
	}
	// Apply callback provenance as a separate post-hardening deployment wave.
	if err = Up(ctx, c, loaded); err != nil {
		t.Fatal(err)
	}
	var distinctReceipts bool
	if err = c.QueryRow(ctx, `SELECT
	 EXISTS(SELECT 1 FROM schema_migrations WHERE version=900004 AND name='refund_dispatch_fencing')
	 AND EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1 AND name='pricing_operation_receipts' AND checksum=$2)
	 AND to_regclass('pricing_operation_commands') IS NOT NULL
	 AND EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='refunds' AND column_name='dispatch_lease_until')`, pricingReceiptVersion, pricingReceiptChecksum).Scan(&distinctReceipts); err != nil || !distinctReceipts {
		t.Fatal("refund dispatch and pricing receipts require distinct frozen migration identities", err)
	}
	var notificationAfter string
	var unbound bool
	if err = fixture.QueryRow(ctx, notificationFacts).Scan(&notificationAfter); err != nil || notificationAfter != oldNotification {
		t.Fatal("legacy callback facts changed", err)
	}
	if err = fixture.QueryRow(ctx, `SELECT provider_id IS NULL AND provider_version IS NULL FROM payment_notifications`).Scan(&unbound); err != nil || !unbound {
		t.Fatal("invented legacy callback verifier provenance", err)
	}
	var guessedDefaults int
	if err = fixture.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='payment_notifications' AND column_name IN ('provider_id','provider_version') AND (column_default IS NOT NULL OR is_nullable<>'YES')`).Scan(&guessedDefaults); err != nil || guessedDefaults != 0 {
		t.Fatal("callback provenance is not nullable without defaults", err)
	}
	var afterEvidence string
	if err = fixture.QueryRow(ctx, evidence).Scan(&afterEvidence); err != nil || beforeEvidence != afterEvidence {
		t.Fatal("legacy financial evidence changed", err)
	}
	var validated bool
	if err = fixture.QueryRow(ctx, `SELECT convalidated FROM pg_constraint WHERE conrelid='chargeback_cases'::regclass AND conname='chargeback_cases_status_check'`).Scan(&validated); err != nil || validated {
		t.Fatal("legacy constraint must remain NOT VALID", err)
	}
	var bad int
	if err = fixture.QueryRow(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND pg_get_userbyid(c.relowner)='billing_service'`).Scan(&bad); err != nil || bad != 0 {
		t.Fatal("runtime retained ownership", err)
	}
	if err = fixture.QueryRow(ctx, `SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public' AND pg_get_userbyid(p.proowner)='billing_service'`).Scan(&bad); err != nil || bad != 0 {
		t.Fatal("runtime retained function ownership", err)
	}
	config.User = "billing_service"
	runtime, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(context.Background())
	for _, sql := range []string{
		`TRUNCATE ledger_postings`, `ALTER TABLE journal_entries DISABLE TRIGGER ALL`,
		`DROP TABLE payment_evidence CASCADE`, `CREATE TABLE public.unauthorized(id integer)`,
		`INSERT INTO schema_migrations(version,name,checksum) VALUES(777,'forged','forged')`,
		`SET ROLE billing_schema_owner`,
		`UPDATE chargeback_cases SET revoked_base_microcredits=4 WHERE status='RESOLVED'`,
	} {
		if _, e := runtime.Exec(ctx, sql); e == nil {
			t.Fatalf("unsafe runtime SQL succeeded: %s", sql)
		}
	}

	assertSQLState := func(connection *pgx.Conn, sql, want string) {
		t.Helper()
		_, e := connection.Exec(ctx, sql)
		var pgError *pgconn.PgError
		if !errors.As(e, &pgError) || pgError.Code != want {
			t.Fatalf("SQL state wanted %s got %v", want, e)
		}
	}
	assertSQLState(runtime, `UPDATE chargeback_cases SET revoked_base_microcredits=4 WHERE status='RESOLVED'`, "23514")
	assertSQLState(runtime, `INSERT INTO chargeback_cases SELECT '99999999-9999-4999-8999-999999999999',payment_evidence_id,payment_order_id,payment_transaction_id,entitlement_package_id,wallet_id,'forbidden-new',status,revoked_base_microcredits,revoked_bonus_microcredits,receivable_id,created_at,resolved_at,manual_review_case_id,resolution_code,resolution_reason,version FROM chargeback_cases`, "23514")
	assertSQLState(fixture, `TRUNCATE ledger_postings`, "55000")
	if _, err = fixture.Exec(ctx, `BEGIN; SET LOCAL session_replication_role=replica;
INSERT INTO entitlement_packages(id,user_id,wallet_id,payment_order_id,credit_lot_id,product_id,plan_name,sku_code,amount_fen,points_total,points_remaining,credited_at,valid_from,valid_until,status,created_at,updated_at,source_type,source_reference)
VALUES('88888888-8888-4888-8888-888888888888',NULL,'ffffffff-ffff-4fff-8fff-ffffffffffff',NULL,'77777777-7777-4777-8777-777777777777','fixture','fixture','fixture',0,1,1,'2026-01-01','2026-01-01','2026-02-01','ACTIVE','2026-01-01','2026-01-01','SYSTEM','fixture-validity');COMMIT;`); err != nil {
		t.Fatal(err)
	}
	assertSQLState(runtime, `UPDATE entitlement_packages SET valid_until=valid_from WHERE id='88888888-8888-4888-8888-888888888888'`, "23514")
	var execute bool
	if err = runtime.QueryRow(ctx, `SELECT has_function_privilege(current_user,'public.set_recharge_product_lifecycle(uuid,bigint,text,timestamptz)','EXECUTE')`).Scan(&execute); err != nil || !execute {
		t.Fatal("lifecycle function privilege", err)
	}
	// A future lower-numbered wave must be applied despite the higher fence, then
	// its new table/function and schema authority must be revoked in the same tx.
	next := migrations.Migration{Version: 70, Name: "owned_incremental_probe", UpSQL: `CREATE TABLE public.owner_incremental_probe(id integer PRIMARY KEY); CREATE FUNCTION public.owner_incremental_trigger() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;`}
	// Generic source checksum is only metadata; VerifyApplied recomputes SQL bytes.
	next.Checksum = sqlChecksum(next.UpSQL)
	expanded := append(append([]migrations.Migration{}, loaded...), next)
	if err = Up(ctx, c, expanded); err != nil {
		t.Fatal(err)
	}
	if _, err = runtime.Exec(ctx, `INSERT INTO owner_incremental_probe VALUES(1)`); err != nil {
		t.Fatal("future runtime DML", err)
	}
	if _, err = runtime.Exec(ctx, `ALTER TABLE owner_incremental_probe ADD COLUMN forbidden integer`); err == nil {
		t.Fatal("future table owner leaked")
	}
	if err = Up(ctx, c, expanded); err != nil {
		t.Fatal("repeat migration", err)
	}

	failing := migrations.Migration{Version: 71, Name: "rollback_owned_probe", UpSQL: `CREATE TABLE public.must_rollback(id integer); SELECT 1/0;`}
	failing.Checksum = sqlChecksum(failing.UpSQL)
	if err = Up(ctx, c, append(append([]migrations.Migration{}, expanded...), failing)); err == nil {
		t.Fatal("failing future migration unexpectedly passed")
	}
	var leaked bool
	if err = fixture.QueryRow(ctx, `SELECT to_regclass('public.must_rollback') IS NOT NULL OR EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND pg_get_userbyid(c.relowner)='billing_service') OR has_schema_privilege('billing_service','public','CREATE')`).Scan(&leaked); err != nil || leaked {
		t.Fatal("failed increment leaked authority", err)
	}
	var role string
	if err = c.QueryRow(ctx, `SELECT current_user`).Scan(&role); err != nil || role != "billing_service" {
		t.Fatal("migration changed caller identity", err)
	}
	if _, err = migrations.VerifyApplied(ctx, c, expanded); err != nil {
		t.Fatal(err)
	}
}
