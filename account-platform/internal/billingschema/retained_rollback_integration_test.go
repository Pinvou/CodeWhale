package billingschema

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The old numeric-only history is an explicit legacy fixture, not a normal
// completed current deployment (whose latest migration is forward-only).
// Original SQL runs under the existing exclusive non-superuser actors.
func TestF02RetainedRollbackBoundariesPostgres(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("exclusive actual PostgreSQL schema fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	load := func(domain string) []migrations.Migration {
		t.Helper()
		items, err := migrations.Load("../../migrations/" + domain)
		if err != nil {
			t.Fatal(err)
		}
		return items
	}
	account, billing := load("account"), load("billing")
	legacy := func(domain string, through int64, loaded []migrations.Migration) *pgx.Conn {
		t.Helper()
		owner := "account_service"
		if domain == "billing" {
			owner = "billing_schema_owner"
		}
		c := f.database(t, ctx, domain, owner)
		var subset []migrations.Migration
		for _, m := range loaded {
			if m.Version <= through {
				subset = append(subset, m)
			}
		}
		if err := migrations.Up(ctx, c, subset); err != nil {
			t.Fatal("original numeric-only legacy fixture", err)
		}
		return c
	}
	unchanged := func(c *pgx.Conn, beforeShape, beforeLedger string) {
		t.Helper()
		if beforeShape != stageShape(t, ctx, c) || beforeLedger != stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`) {
			t.Fatal("rejected rollback changed schema/data history")
		}
	}
	t.Run("account033-statement-immutable-even-no-matching-policy", func(t *testing.T) {
		c := legacy("account", 33, account)
		if _, err := c.Exec(ctx, `DELETE FROM approval_command_policies WHERE command_type='OWNED_NO_MATCH'`); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("zero-row statement trigger outcome: %v", err)
		}
		shape, ledger := stageShape(t, ctx, c), stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`)
		if err := migrations.DownOne(ctx, c, account); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("unguarded original statement trigger outcome: %v", err)
		}
		unchanged(c, shape, ledger)
		if err := accountschema.DownOne(ctx, c, account); err == nil || !strings.Contains(err.Error(), "append-only policy history") {
			t.Fatalf("public Account guard outcome: %v", err)
		}
		unchanged(c, shape, ledger)
	})
	t.Run("billing025-does-not-restore-original-enum", func(t *testing.T) {
		old := legacy("billing", 25, billing)
		if err := migrations.DownOne(ctx, old, billing); err != nil {
			t.Fatal("empty legacy unguarded rollback", err)
		}
		var definition string
		if err := old.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='journal_entries'::regclass AND conname='journal_entries_entry_type_check'`).Scan(&definition); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(definition, "EXPIRY") || strings.Contains(definition, "EXPIRE") || strings.Contains(definition, "REPAIR_CREDIT") {
			t.Fatalf("original enum mismatch not reproduced: %s", definition)
		}
		c := legacy("billing", 25, billing)
		shape, ledger := stageShape(t, ctx, c), stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`)
		if err := DownOne(ctx, c, billing); err == nil || !strings.Contains(err.Error(), "prior journal entry contract") {
			t.Fatalf("public Billing enum guard outcome: %v", err)
		}
		unchanged(c, shape, ledger)
	})
	t.Run("billing029-retains-matching-events", func(t *testing.T) {
		c := legacy("billing", 29, billing)
		if _, err := c.Exec(ctx, `INSERT INTO manual_review_cases(id,domain,case_type,resource_type,resource_id,severity,status,created_at,claim_due_at,first_response_due_at,mitigation_due_at,resolution_due_at,updated_at) VALUES('91111111-1111-4111-8111-111111111111','BILLING','PAYMENT_ANOMALY','PAYMENT_ORDER','21111111-1111-4111-8111-111111111111','P1','OPEN','2026-01-01','2026-01-02','2026-01-02','2026-01-03','2026-01-04','2026-01-01'); INSERT INTO manual_review_case_events(id,case_id,event_type,details,created_at) VALUES('92222222-2222-4222-8222-222222222222','91111111-1111-4111-8111-111111111111','CREATED','{}','2026-01-01')`); err != nil {
			t.Fatal(err)
		}
		shape, ledger := stageShape(t, ctx, c), stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`)
		event := stageValue(t, ctx, c, `SELECT to_jsonb(e)::text FROM manual_review_case_events e`)
		if err := migrations.DownOne(ctx, c, billing); err == nil || !strings.Contains(err.Error(), "events are immutable") {
			t.Fatalf("unguarded original event rejection: %v", err)
		}
		unchanged(c, shape, ledger)
		if err := DownOne(ctx, c, billing); err == nil || !strings.Contains(err.Error(), "immutable manual review events") {
			t.Fatalf("public event guard outcome: %v", err)
		}
		unchanged(c, shape, ledger)
		if event != stageValue(t, ctx, c, `SELECT to_jsonb(e)::text FROM manual_review_case_events e`) {
			t.Fatal("immutable event changed")
		}
	})
	t.Run("billing029-without-events-retains-safe-rollback", func(t *testing.T) {
		c := legacy("billing", 29, billing)
		if err := DownOne(ctx, c, billing); err != nil {
			t.Fatal("event-free original rollback was blocked", err)
		}
		if stageValue(t, ctx, c, `SELECT count(*)::text FROM schema_migrations WHERE version=29`) != "0" {
			t.Fatal("successful rollback retained wrong ledger version")
		}
	})
	t.Run("billing023-projected-empty-sku-collision", func(t *testing.T) {
		c := legacy("billing", 23, billing)
		for _, id := range []string{"OWNED_EMPTY_A", "OWNED_EMPTY_B"} {
			if _, err := c.Exec(ctx, `INSERT INTO recharge_products SELECT (jsonb_populate_record(NULL::recharge_products,(SELECT to_jsonb(p) FROM recharge_products p WHERE product_id='PLAN_TRIAL')||jsonb_build_object('product_id',$1::text,'sku_code',''))).*`, id); err != nil {
				t.Fatal(err)
			}
		}
		shape, ledger := stageShape(t, ctx, c), stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`)
		products := stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(p) ORDER BY product_id)::text FROM recharge_products p`)
		err := migrations.DownOne(ctx, c, billing)
		var uniqueError *pgconn.PgError
		if !errors.As(err, &uniqueError) || uniqueError.Code != "23505" {
			t.Fatalf("original 023 unique index collision must be SQLSTATE 23505: %v", err)
		}
		unchanged(c, shape, ledger)
		if err := DownOne(ctx, c, billing); err == nil || !strings.Contains(err.Error(), "duplicate enabled SKUs") {
			t.Fatalf("projected uniqueness guard outcome: %v", err)
		}
		unchanged(c, shape, ledger)
		if products != stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(p) ORDER BY product_id)::text FROM recharge_products p`) {
			t.Fatal("guard changed catalog rows")
		}
	})
	t.Run("billing023-mapped-catalog-retains-safe-rollback", func(t *testing.T) {
		c := legacy("billing", 23, billing)
		if err := DownOne(ctx, c, billing); err != nil {
			t.Fatal("collision-free original rollback was blocked", err)
		}
	})
}

func TestF02ExecutionGuardAtomicMigrationPostgres(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("exclusive actual PostgreSQL schema fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	var before53 []migrations.Migration
	var original53, guard migrations.Migration
	for _, m := range loaded {
		if m.Version < 53 || m.Version == 900001 {
			before53 = append(before53, m)
		}
		if m.Version == 53 {
			original53 = m
		}
		if m.Version == 900002 {
			guard = m
		}
	}
	prepare := func() *pgx.Conn {
		t.Helper()
		c := f.database(t, ctx, "account", "account_service")
		if err := accountschema.Up(ctx, c, before53); err != nil {
			t.Fatal("owned pre-053 schema", err)
		}
		return c
	}
	t.Run("later-migration-failure-never-commits-weakened-facts", func(t *testing.T) {
		c := prepare()
		fault := append([]migrations.Migration(nil), loaded...)
		found := false
		for i := range fault {
			if fault[i].Version == 54 {
				fault[i].UpSQL = "SELECT 1/0;"
				found = true
			}
		}
		if !found {
			t.Fatal("actual next migration 054 is absent")
		}
		// Only the in-memory next-migration SQL is a controlled fault. No
		// checked-in SQL, version, or original 053/900002 source is changed.
		if err := accountschema.Up(ctx, c, fault); err == nil || !strings.Contains(err.Error(), "migration 54") {
			t.Fatalf("controlled next-migration failure: %v", err)
		}
		if stageValue(t, ctx, c, `SELECT count(*)::text FROM schema_migrations WHERE version=53`) != "1" || stageValue(t, ctx, c, `SELECT count(*)::text FROM schema_migrations WHERE version=900002`) != "0" {
			t.Fatal("bridge falsely recorded 900002 or failed to commit 053")
		}
		assertExecutionFactsGuard(t, ctx, c)
		if err := accountschema.Up(ctx, c, loaded); err != nil {
			t.Fatal("resume original public Up", err)
		}
		if _, err := migrations.VerifyApplied(ctx, c, loaded); err != nil {
			t.Fatal("complete original ledger verification", err)
		}
		assertExecutionFactsGuard(t, ctx, c)
	})
	t.Run("controlled-after-hook-failure-rolls-back-original053", func(t *testing.T) {
		c := prepare()
		shape := stageShape(t, ctx, c)
		ledger := stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`)
		// Exercise the migration engine's actual transaction with the same
		// frozen guard SQL, then a controlled SQL error. This is a hook fault
		// diagnostic, not a simulated business or live execution failure.
		err := migrations.UpWithHooks(ctx, c, []migrations.Migration{original53}, migrations.UpHooks{AfterApply: func(ctx context.Context, tx pgx.Tx, m migrations.Migration) error {
			if _, err := tx.Exec(ctx, guard.UpSQL); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `SELECT 1/0`)
			return err
		}})
		if err == nil || !strings.Contains(err.Error(), "division by zero") {
			t.Fatalf("controlled hook SQL failure: %v", err)
		}
		if shape != stageShape(t, ctx, c) || ledger != stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`) {
			t.Fatal("hook failure committed 053 DDL or ledger")
		}
	})
}

func assertExecutionFactsGuard(t *testing.T, ctx context.Context, c *pgx.Conn) {
	t.Helper()
	// Attach the actual production function to an isolated row with its real
	// fields. No authorization/session is fabricated to insert a live execution.
	if _, err := c.Exec(ctx, `CREATE TEMP TABLE IF NOT EXISTS owned_execution_fact_probe (LIKE approval_executions INCLUDING DEFAULTS); DROP TRIGGER IF EXISTS owned_facts_guard ON owned_execution_fact_probe; CREATE TRIGGER owned_facts_guard BEFORE UPDATE ON owned_execution_fact_probe FOR EACH ROW EXECUTE FUNCTION public.protect_approval_execution(); INSERT INTO owned_execution_fact_probe(id,approval_request_id,command_type,command_fingerprint,execution_idempotency_key,executor_identity,job_run_id,status,authorization_snapshot,critical_resource_versions,financial_snapshot,started_at) SELECT '11111111-1111-4111-8111-111111111111','22222222-2222-4222-8222-222222222222','OWNED_PROBE',decode(repeat('a',64),'hex'),'owned-facts-key','billing-service','owned-job','EXECUTING','{}','{}','{}',now() WHERE NOT EXISTS(SELECT 1 FROM owned_execution_fact_probe)`); err != nil {
		t.Fatal("actual function probe fixture", err)
	}
	for _, field := range []string{"critical_resource_versions", "financial_snapshot"} {
		if _, err := c.Exec(ctx, "UPDATE owned_execution_fact_probe SET "+field+"='{\"changed\":true}'"); err == nil || !strings.Contains(err.Error(), "identity is immutable") {
			t.Fatalf("frozen %s changed: %v", field, err)
		}
	}
	if _, err := c.Exec(ctx, `UPDATE owned_execution_fact_probe SET status='FAILED',error_class='OWNED_RETRY'`); err != nil {
		t.Fatal("legal status/error retry fields blocked", err)
	}
}
