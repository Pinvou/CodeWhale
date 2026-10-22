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
		if err := DownOne(ctx, c, loaded); err == nil {
			t.Fatal("forward-only receipt migration rolled back")
		}
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
		for _, checksum := range []string{originalWalletReceiptChecksum, originalWalletReceiptCRLFChecksum} {
			t.Run(checksum[:8], func(t *testing.T) {
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
			})
		}
	})
	t.Run("unknown-original053-and-shape-rejected-without-overwrite", func(t *testing.T) {
		for _, scenario := range []string{"unknown-no-table", "unknown-with-table", "known-wrong-shape"} {
			t.Run(scenario, func(t *testing.T) {
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
