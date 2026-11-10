package billingschema

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestTokenWalletReceiptMigrationIdentity(t *testing.T) {
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"changed-up", "forged-checksum", "changed-down", "moved-version"} {
		t.Run(kind, func(t *testing.T) {
			changed := append([]migrations.Migration(nil), loaded...)
			for i := range changed {
				if changed[i].Version == walletReceiptVersion {
					switch kind {
					case "changed-up":
						changed[i].UpSQL += "\nSELECT 1;"
					case "forged-checksum":
						changed[i].UpSQL += "\nSELECT 1;"
						changed[i].Checksum = sqlChecksum(changed[i].UpSQL)
					case "changed-down":
						changed[i].DownSQL = "SELECT 1;"
					case "moved-version":
						changed[i].Version++
					}
				}
			}
			if _, err := Ordered(changed); err == nil {
				t.Fatal("changed wallet receipt identity accepted")
			}
		})
	}
}

func TestTokenWalletReceiptSchemaPostgres(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("actual dedicated PostgreSQL schema fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("fresh-public-migration-runtime-append-only-and-forward-only", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
		defer cancel()
		c := f.database(t, ctx, "billing", "billing_schema_owner")
		if err := Up(ctx, c, loaded); err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyApplied(ctx, c, loaded); err != nil {
			t.Fatal("fresh exact history rejected", err)
		}
		var original bool
		if err := c.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=53)`).Scan(&original); err != nil || original {
			t.Fatal("fresh history invented original053", err)
		}
		runtime, err := pgx.Connect(ctx, f.serviceURL(t, c, "billing_service"))
		if err != nil {
			t.Fatal(err)
		}
		defer runtime.Close(context.Background())
		var owner string
		var read, insert, update, del, trunc, ddl, super bool
		if err := runtime.QueryRow(ctx, `SELECT pg_get_userbyid(relowner),has_table_privilege('billing_service',oid,'SELECT'),has_table_privilege('billing_service',oid,'INSERT'),has_table_privilege('billing_service',oid,'UPDATE'),has_table_privilege('billing_service',oid,'DELETE'),has_table_privilege('billing_service',oid,'TRUNCATE'),has_schema_privilege('billing_service','public','CREATE'),(SELECT rolsuper FROM pg_roles WHERE rolname=current_user) FROM pg_class WHERE oid='public.token_account_wallet_provisioning_receipts'::regclass`).Scan(&owner, &read, &insert, &update, &del, &trunc, &ddl, &super); err != nil || owner != "billing_schema_owner" || !read || !insert || update || del || trunc || ddl || super {
			t.Fatalf("runtime owner%v R/I/U/D/T/DDL/super %v/%v/%v/%v/%v/%v/%v err%v", owner, read, insert, update, del, trunc, ddl, super, err)
		}
		for _, q := range []string{`UPDATE token_account_wallet_provisioning_receipts SET status='FAILED'`, `DELETE FROM token_account_wallet_provisioning_receipts`, `TRUNCATE token_account_wallet_provisioning_receipts`, `ALTER TABLE token_account_wallet_provisioning_receipts ADD COLUMN unsafe text`, `SET ROLE billing_schema_owner`} {
			if _, err := runtime.Exec(ctx, q); err == nil {
				t.Fatal("runtime obtained forbidden authority", q)
			}
		}
		before := stageShape(t, ctx, c)
		ledger := stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(s) ORDER BY version)::text FROM schema_migrations s`)
		assertHistoricalBillingRollback(t, ctx, f.database(t, ctx, "billing", "billing_schema_owner"), loaded, 900023, "P0001", "wallet receipt history is forward-only")
		assertBillingCurrentReplay(t, ctx, c, loaded)
		migrations.Unlock(ctx, c)
		if before != stageShape(t, ctx, c) || ledger != stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(s) ORDER BY version)::text FROM schema_migrations s`) {
			t.Fatal("failed rollback changed shape or ledger")
		}
		// The generic verifier remains strict even for an otherwise legitimate extra
		// migration. This case never changes application data or the receipt table.
		if _, err := c.Exec(ctx, `SET ROLE billing_schema_owner; INSERT INTO schema_migrations(version,name,checksum) VALUES(1900023,'owned-negative-control',repeat('a',64)); SET ROLE billing_service`); err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyApplied(ctx, c, loaded); err == nil {
			t.Fatal("unknown extra history accepted")
		}
	})
	t.Run("original-053-LF-CRLF-and-existing-receipts-preserved", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
		defer cancel()
		// This actor is exclusively owned by this fixture. Never modify a preexisting
		// deployment role or add runtime membership in the historical table owner.
		var exists bool
		if err := f.admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='billing_wallet_receipt_deployer')`).Scan(&exists); err != nil || exists {
			t.Fatal("legacy actor must be absent", err)
		}
		if _, err := f.admin.Exec(ctx, `CREATE ROLE billing_wallet_receipt_deployer NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS; GRANT billing_wallet_receipt_deployer TO billing_migrator`); err != nil {
			t.Fatal(err)
		}
		f.roles = append(f.roles, "billing_wallet_receipt_deployer")
		for _, scenario := range []struct{ checksum, owner string }{
			{originalWalletReceiptChecksum, "deployer"},
			{originalWalletReceiptCRLFChecksum, "deployer"},
			{originalWalletReceiptChecksum, "schema-owner"},
			{originalWalletReceiptCRLFChecksum, "schema-owner"},
			{originalWalletReceiptChecksum, "schema-owner-hardening-crlf"},
			{originalWalletReceiptChecksum, "schema-owner-extra-trigger"},
			{originalWalletReceiptChecksum, "schema-owner-altered-guard"},
			{originalWalletReceiptChecksum, "schema-owner-pending-replaced-trigger"},
			{originalWalletReceiptChecksum, "schema-owner-pending-altered-guard"},
			{originalWalletReceiptChecksum, "schema-owner-pending-missing-standard-extra-trigger"},
			{originalWalletReceiptChecksum, "schema-owner-pending-missing-standard-altered-guard"},
			{originalWalletReceiptChecksum, "schema-owner-pending-replaced-original-guard"},
			{originalWalletReceiptChecksum, "schema-owner-pending-missing-standard-replaced-original-guard"},
			{originalWalletReceiptChecksum, "runtime-owner"},
			{originalWalletReceiptChecksum, "runtime-owner-unreviewed-standard"},
		} {
			checksum := scenario.checksum
			t.Run(scenario.owner+"-"+checksum[:8], func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
				defer cancel()
				c := f.database(t, ctx, "billing", "billing_schema_owner")
				var base []migrations.Migration
				for _, m := range loaded {
					if (m.Version <= 52) || (m.Version >= 900001 && m.Version <= 900012 && m.Version != 900003) {
						base = append(base, m)
					}
				}
				if err := Up(ctx, c, base); err != nil {
					t.Fatal("real original predecessor", err)
				}
				legacy := migrations.Migration{Version: 53, Name: "token_account_wallet_provisioning_receipts", UpSQL: originalWalletReceiptSQL, DownSQL: "SELECT 1;", Checksum: checksum}
				if checksum == originalWalletReceiptCRLFChecksum {
					legacy.UpSQL = strings.ReplaceAll(originalWalletReceiptSQL, "\n", "\r\n")
				}
				if err := migrations.Up(ctx, c, []migrations.Migration{legacy}); err != nil {
					t.Fatal("original published053", err)
				}
				// Real old runtime creates one wallet and immutable receipt before upgrade.
				repo, err := billing.NewPostgresRepository(ctx, f.serviceURL(t, c, "billing_service"))
				if err != nil {
					t.Fatal(err)
				}
				defer repo.Close()
				owner, ref := uuid.New(), uuid.New()
				wallet, _, err := repo.EnsureWallet(ctx, "PERSONAL", owner)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := c.Exec(ctx, `INSERT INTO token_account_wallet_provisioning_receipts(command_reference,owner_type,owner_id,request_digest,status,wallet_id,attempt_count,created_at,completed_at) VALUES($1,'PERSONAL',$2,repeat('a',64),'SUCCEEDED',$3,1,'2026-01-01','2026-01-01')`, ref, owner, wallet.ID); err != nil {
					t.Fatal(err)
				}
				before := stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(v) ORDER BY command_reference)::text FROM token_account_wallet_provisioning_receipts v`)
				if scenario.owner != "deployer" {
					control, err := pgx.Connect(ctx, f.serviceURL(t, c, f.config.User))
					if err != nil {
						t.Fatal(err)
					}
					defer control.Close(context.Background())
					ownerRole := "billing_schema_owner"
					if strings.HasPrefix(scenario.owner, "runtime-owner") {
						ownerRole = "billing_service"
					}
					if _, err := control.Exec(ctx, `ALTER TABLE public.token_account_wallet_provisioning_receipts OWNER TO `+pgx.Identifier{ownerRole}.Sanitize()+`;
ALTER FUNCTION public.reject_token_account_wallet_receipt_mutation() OWNER TO `+pgx.Identifier{ownerRole}.Sanitize()); err != nil {
						t.Fatal(err)
					}
					if scenario.owner == "runtime-owner-unreviewed-standard" {
						if _, err := control.Exec(ctx, `CREATE FUNCTION public.unreviewed_receipt_guard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$;
CREATE TRIGGER billing_no_truncate BEFORE TRUNCATE ON public.token_account_wallet_provisioning_receipts FOR EACH STATEMENT EXECUTE FUNCTION public.unreviewed_receipt_guard()`); err != nil {
							t.Fatal(err)
						}
						shape := stageShape(t, ctx, c)
						ledger := stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(s) ORDER BY version)::text FROM schema_migrations s`)
						if err := Up(ctx, c, loaded); err == nil || !strings.Contains(err.Error(), "unreviewed hardening guard") {
							t.Fatal("first hardening accepted an unknown runtime-owned guard", err)
						}
						if shape != stageShape(t, ctx, c) || ledger != stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(s) ORDER BY version)::text FROM schema_migrations s`) || before != stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(v) ORDER BY command_reference)::text FROM token_account_wallet_provisioning_receipts v`) {
							t.Fatal("first hardening rejection changed schema, history, or receipts")
						}
						return
					}
					withHardening := append([]migrations.Migration(nil), base...)
					for _, m := range loaded {
						if m.Version == 900003 {
							withHardening = append(withHardening, m)
						}
					}
					if err := Up(ctx, c, withHardening); err != nil {
						t.Fatal("public hardening of restored schema-owned receipt", err)
					}
					if scenario.owner == "schema-owner-hardening-crlf" {
						if _, err := control.Exec(ctx, `UPDATE public.schema_migrations SET checksum=$1 WHERE version=900003`, walletReceiptHardeningCRLFChecksum); err != nil {
							t.Fatal(err)
						}
					}
					if strings.HasSuffix(scenario.owner, "extra-trigger") || strings.HasSuffix(scenario.owner, "altered-guard") || strings.HasSuffix(scenario.owner, "replaced-trigger") || strings.HasSuffix(scenario.owner, "replaced-original-guard") {
						var withoutReceipt []migrations.Migration
						for _, m := range loaded {
							if m.Version != walletReceiptVersion {
								withoutReceipt = append(withoutReceipt, m)
							}
						}
						if !strings.Contains(scenario.owner, "-pending-") {
							if err := Up(ctx, c, withoutReceipt); err != nil {
								t.Fatal(err)
							}
						}
						mutation := `CREATE FUNCTION public.unreviewed_receipt_guard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$;
CREATE TRIGGER unreviewed_receipt_guard BEFORE TRUNCATE ON public.token_account_wallet_provisioning_receipts FOR EACH STATEMENT EXECUTE FUNCTION public.unreviewed_receipt_guard()`
						if strings.HasSuffix(scenario.owner, "altered-guard") {
							mutation = `CREATE OR REPLACE FUNCTION public.reject_billing_truncate() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$ BEGIN RETURN NULL; END $$`
						}
						if strings.HasSuffix(scenario.owner, "replaced-trigger") {
							mutation = `CREATE FUNCTION public.unreviewed_receipt_guard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$;
DROP TRIGGER billing_no_truncate ON public.token_account_wallet_provisioning_receipts;
CREATE TRIGGER billing_no_truncate BEFORE TRUNCATE ON public.token_account_wallet_provisioning_receipts FOR EACH STATEMENT EXECUTE FUNCTION public.unreviewed_receipt_guard()`
						}
						if strings.Contains(scenario.owner, "missing-standard") {
							mutation += `; DROP TRIGGER billing_no_truncate ON public.token_account_wallet_provisioning_receipts`
						}
						if strings.HasSuffix(scenario.owner, "replaced-original-guard") {
							mutation = `CREATE FUNCTION public.unreviewed_receipt_guard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$;
DROP TRIGGER token_account_wallet_receipt_immutable ON public.token_account_wallet_provisioning_receipts;
CREATE TRIGGER token_account_wallet_receipt_immutable BEFORE UPDATE OR DELETE ON public.token_account_wallet_provisioning_receipts FOR EACH ROW EXECUTE FUNCTION public.unreviewed_receipt_guard()`
							if strings.Contains(scenario.owner, "missing-standard") {
								mutation += `; DROP TRIGGER billing_no_truncate ON public.token_account_wallet_provisioning_receipts`
							}
						}
						if _, err := control.Exec(ctx, mutation); err != nil {
							t.Fatal(err)
						}
						shape := stageShape(t, ctx, c)
						ledger := stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(s) ORDER BY version)::text FROM schema_migrations s`)
						if err := Up(ctx, c, loaded); err == nil || !strings.Contains(err.Error(), "unreviewed hardening guard") {
							t.Fatal("unknown guard accepted", err)
						}
						if shape != stageShape(t, ctx, c) || ledger != stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(s) ORDER BY version)::text FROM schema_migrations s`) || before != stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(v) ORDER BY command_reference)::text FROM token_account_wallet_provisioning_receipts v`) {
							t.Fatal("rejected guard changed schema, history, or receipts")
						}
						return
					}
				}
				if err := Up(ctx, c, loaded); err != nil {
					t.Fatal("public upgrade from original053", err)
				}
				if before != stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(v) ORDER BY command_reference)::text FROM token_account_wallet_provisioning_receipts v`) {
					t.Fatal("original receipt changed")
				}
				var got string
				if err := c.QueryRow(ctx, `SELECT checksum FROM schema_migrations WHERE version=53`).Scan(&got); err != nil || got != checksum {
					t.Fatal("original053 ledger rewritten", err)
				}
				if _, err := VerifyApplied(ctx, c, loaded); err != nil {
					t.Fatal("explicit original053 compatible history rejected", err)
				}
				if _, err := migrations.VerifyApplied(ctx, c, loaded); err == nil {
					t.Fatal("generic exact verifier was weakened")
				}
				if scenario.owner == "schema-owner" || scenario.owner == "schema-owner-hardening-crlf" {
					if got := stageValue(t, ctx, c, `SELECT count(*) FROM pg_trigger WHERE tgrelid='public.token_account_wallet_provisioning_receipts'::regclass AND NOT tgisinternal`); got != "2" {
						t.Fatal("both immutable and truncate guards must survive", got)
					}
				}
			})
		}
	})
	t.Run("unknown-original053-and-shape-rejected-without-overwrite", func(t *testing.T) {
		for _, scenario := range []string{"unknown-no-table", "unknown-with-table", "known-wrong-shape"} {
			t.Run(scenario, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
				defer cancel()
				c := f.database(t, ctx, "billing", "billing_schema_owner")
				var base []migrations.Migration
				for _, m := range loaded {
					if m.Version != walletReceiptVersion && (scenario == "known-wrong-shape" || m.Version != 900022) {
						base = append(base, m)
					}
				}
				if err := Up(ctx, c, base); err != nil {
					t.Fatal(err)
				}
				if _, err := c.Exec(ctx, `SET ROLE billing_schema_owner`); err != nil {
					t.Fatal(err)
				}
				if scenario != "unknown-no-table" {
					if _, err := c.Exec(ctx, `CREATE TABLE token_account_wallet_provisioning_receipts(unsafe text)`); err != nil {
						t.Fatal(err)
					}
				}
				checksum := strings.Repeat("b", 64)
				if scenario == "known-wrong-shape" {
					checksum = originalWalletReceiptChecksum
				}
				if _, err := c.Exec(ctx, `INSERT INTO schema_migrations(version,name,checksum) VALUES(53,'owned-history-fixture',$1)`, checksum); err != nil {
					t.Fatal(err)
				}
				if _, err := c.Exec(ctx, `SET ROLE billing_service`); err != nil {
					t.Fatal(err)
				}
				before := stageShape(t, ctx, c)
				ledger := stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(s) ORDER BY version)::text FROM schema_migrations s`)
				expected := "not the original history"
				if scenario == "known-wrong-shape" {
					expected = "shape does not match"
				}
				if err := Up(ctx, c, loaded); err == nil || !strings.Contains(err.Error(), expected) {
					t.Fatal("invalid legacy table/history accepted", scenario, err)
				}
				if _, err := VerifyApplied(ctx, c, loaded); err == nil {
					t.Fatal("incomplete or unknown053 verification accepted")
				}
				if before != stageShape(t, ctx, c) || ledger != stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(s) ORDER BY version)::text FROM schema_migrations s`) {
					t.Fatal("failed compatibility changed schema/ledger", scenario)
				}
			})
		}
	})
}
