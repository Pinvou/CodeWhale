package billingschema

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

func TestManualRetentionHistoricalHardeningPostgres(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("actual exclusive PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	c := f.database(t, ctx, "billing", "billing_schema_owner")
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	var predecessor []migrations.Migration
	for _, m := range loaded {
		if m.Version != 32 {
			predecessor = append(predecessor, m)
		}
	}
	if err := Up(ctx, c, predecessor); err != nil {
		t.Fatal(err)
	}
	definition := stageValue(t, ctx, c, `SELECT pg_get_functiondef('public.purge_product_manual_admission_facts()'::regprocedure)`)
	exec := func(sql string) {
		t.Helper()
		if _, err := c.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := func() string {
		return stageValue(t, ctx, c, `SELECT jsonb_build_object(
   'function',(SELECT to_jsonb(p) FROM pg_proc p WHERE oid='public.purge_product_manual_admission_facts()'::regprocedure),
   'ledger',(SELECT jsonb_agg(to_jsonb(m) ORDER BY version) FROM schema_migrations m),
   'submissions',(SELECT coalesce(jsonb_agg(to_jsonb(s) ORDER BY verification_id),'[]') FROM product_manual_admission_submissions s),
   'decisions',(SELECT coalesce(jsonb_agg(to_jsonb(d) ORDER BY verification_id,reviewer_id),'[]') FROM product_manual_admission_decisions d),
   'runtime_create',has_schema_privilege('billing_service','public','CREATE'),
   'runtime_write',has_table_privilege('billing_service','product_manual_admission_submissions','UPDATE,DELETE,TRUNCATE'),
   'runtime_membership',pg_has_role('billing_service','billing_schema_owner','MEMBER'),
   'replay_absent',to_regclass('public.event_replay_requests') IS NULL)::text`)
	}
	t.Run("same-name-altered-body-is-not-exempt", func(t *testing.T) {
		exec(`SET ROLE billing_schema_owner; CREATE OR REPLACE FUNCTION public.purge_product_manual_admission_facts() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$ BEGIN RETURN NEW; END $$; SET ROLE billing_service`)
		before := snapshot()
		if err := Up(ctx, c, loaded); err == nil || !strings.Contains(err.Error(), "exact installed owner trigger") {
			t.Fatalf("altered trigger accepted or wrong refusal: %v", err)
		}
		if snapshot() != before {
			t.Fatal("failed identity validation changed metadata, facts, ledger or runtime authority")
		}
		exec("SET ROLE billing_schema_owner; " + definition + "; SET ROLE billing_service")
	})
	t.Run("unknown-definer-still-refused-and-rollback-restores-known-trigger", func(t *testing.T) {
		exec(`SET ROLE billing_schema_owner; CREATE FUNCTION public.manual_retention_unreviewed_probe() RETURNS integer LANGUAGE sql SECURITY DEFINER AS 'SELECT 1'; SET ROLE billing_service`)
		before := snapshot()
		if err := Up(ctx, c, loaded); err == nil || !strings.Contains(err.Error(), "unreviewed billing SECURITY DEFINER function: manual_retention_unreviewed_probe") {
			t.Fatalf("unknown definer accepted or wrong refusal: %v", err)
		}
		if snapshot() != before {
			t.Fatal("failed historical migration changed metadata, facts, ledger or runtime authority")
		}
		exec(`SET ROLE billing_schema_owner; DROP FUNCTION public.manual_retention_unreviewed_probe(); SET ROLE billing_service`)
	})
	if err := Up(ctx, c, loaded); err != nil {
		t.Fatal("next public Up did not recover after rejected historical apply", err)
	}
	if _, err := VerifyApplied(ctx, c, loaded); err != nil {
		t.Fatal(err)
	}
	var restored bool
	if err := c.QueryRow(ctx, `SELECT p.prosecdef AND pg_get_userbyid(p.proowner)='billing_schema_owner'
  AND p.proconfig=ARRAY['search_path=pg_catalog, public']::text[]
  AND NOT has_function_privilege('billing_service',p.oid,'EXECUTE')
  AND NOT has_table_privilege('billing_service','product_manual_admission_submissions','UPDATE,DELETE,TRUNCATE')
  AND NOT has_column_privilege('billing_service','product_manual_admission_submissions','snapshot','UPDATE')
  AND NOT has_column_privilege('billing_service','product_manual_admission_decisions','reason','UPDATE')
  AND NOT pg_has_role('billing_service','billing_schema_owner','MEMBER')
  AND NOT has_schema_privilege('billing_service','public','CREATE')
  FROM pg_proc p WHERE p.oid='public.purge_product_manual_admission_facts()'::regprocedure`).Scan(&restored); err != nil || !restored {
		t.Fatal("known trigger or runtime authority not restored", err)
	}
	before := snapshot()
	if err := Up(ctx, c, loaded); err != nil || snapshot() != before {
		t.Fatal("idempotent next Up changed committed metadata or facts", err)
	}
	t.Run("legacy-CRLF-ledger-replay", func(t *testing.T) {
		legacyDB := f.database(t, ctx, "billing", "billing_schema_owner")
		if err := Up(ctx, legacyDB, predecessor); err != nil {
			t.Fatal(err)
		}
		var manual migrations.Migration
		for _, m := range loaded {
			if m.Version == manualProductAdmissionVersion {
				manual = m
			}
		}
		normalized := strings.ReplaceAll(strings.ReplaceAll(manual.UpSQL, "\r\n", "\n"), "\r", "\n")
		sum := sha256.Sum256([]byte(strings.ReplaceAll(normalized, "\n", "\r\n")))
		if _, err := legacyDB.Exec(ctx, `SET ROLE billing_schema_owner`); err != nil {
			t.Fatal(err)
		}
		if _, err := legacyDB.Exec(ctx, `UPDATE schema_migrations SET checksum=$1 WHERE version=$2`, hex.EncodeToString(sum[:]), manualProductAdmissionVersion); err != nil {
			t.Fatal(err)
		}
		if _, err := legacyDB.Exec(ctx, `SET ROLE billing_service`); err != nil {
			t.Fatal(err)
		}
		before := stageValue(t, ctx, legacyDB, `SELECT to_jsonb(m)::text FROM schema_migrations m WHERE version=900038`)
		if err := Up(ctx, legacyDB, loaded); err != nil {
			t.Fatal("equivalent original CRLF receipt must permit late historical migration", err)
		}
		if _, err := VerifyApplied(ctx, legacyDB, loaded); err != nil {
			t.Fatal(err)
		}
		if before != stageValue(t, ctx, legacyDB, `SELECT to_jsonb(m)::text FROM schema_migrations m WHERE version=900038`) {
			t.Fatal("legacy receipt rewritten")
		}
		if stageValue(t, ctx, legacyDB, `SELECT pg_get_functiondef('public.purge_product_manual_admission_facts()'::regprocedure)`) != definition {
			t.Fatal("legacy replay changed retained trigger definition")
		}
	})

}
