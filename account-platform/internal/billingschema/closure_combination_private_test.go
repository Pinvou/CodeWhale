package billingschema

import (
	"context"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/tokenschema"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestClosurePrivatePublicCombination(t *testing.T) {
	if os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL") == "" {
		t.Skip("explicit isolated schema database required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 160*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL"))
	f.installClosureTokenActors(t, ctx)
	ac := f.database(t, ctx, "account", "account_service")
	bc := f.database(t, ctx, "billing", "billing_schema_owner")
	am, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if err = accountschema.Up(ctx, ac, am); err != nil {
		t.Fatal("Account public Up", err)
	}
	if _, err = migrations.VerifyApplied(ctx, ac, am); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, ac)
	bm, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	if err = Up(ctx, bc, bm); err != nil {
		t.Fatal("Billing public Up", err)
	}
	if _, err = migrations.VerifyApplied(ctx, bc, bm); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, bc)
	name := "f08_token_closure_" + uuid.NewString()[:8] + time.Now().Format("150405")
	var exists bool
	if err = f.admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)`, name).Scan(&exists); err != nil || exists {
		t.Fatal("own Token UUID absence", err)
	}
	if _, err = f.admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	f.dbs[name] = f.config.User
	cfg := f.config.Copy()
	cfg.Database = name
	owner, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Exec(ctx, `CREATE EXTENSION pgcrypto; ALTER SCHEMA public OWNER TO token_schema_owner; REVOKE CREATE ON SCHEMA public FROM PUBLIC; GRANT USAGE,CREATE ON SCHEMA public TO token_gateway`); err != nil {
		owner.Close(ctx)
		t.Fatal(err)
	}
	owner.Close(ctx)
	cfg.User = "token_migrator"
	cfg.Password = ""
	tc, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.connections = append(f.connections, tc)
	if _, err = tc.Exec(ctx, `SET ROLE token_gateway`); err != nil {
		t.Fatal(err)
	}
	tm, err := migrations.Load("../../migrations/token")
	if err != nil {
		t.Fatal(err)
	}
	if err = tokenschema.Up(ctx, tc, tm); err != nil {
		t.Fatal("Token public Up", err)
	}
	if _, err = migrations.VerifyApplied(ctx, tc, tm); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, tc)

	t.Run("cumulative-original-lower-versions-equal-fresh-schema", func(t *testing.T) {
		upgradedAccount := f.database(t, ctx, "account", "account_service")
		var oldAccount []migrations.Migration
		for _, x := range am {
			if x.Version != 27 && x.Version != 28 && x.Version != 900005 {
				oldAccount = append(oldAccount, x)
			}
		}
		if err = accountschema.Up(ctx, upgradedAccount, oldAccount); err != nil {
			t.Fatal("predecessor Account Up", err)
		}
		if _, err = upgradedAccount.Exec(ctx, strings.ReplaceAll(strings.ReplaceAll(stageAccountSeed, "payload,trace_id,occurred_at,next_attempt_at", "payload,trace_id,occurred_at"), "'owned','2026-01-01','2026-01-01');", "'owned','2026-01-01');")); err != nil {
			t.Fatal("existing Account retained data seed", err)
		}
		accountData := stageAccountFacts(t, ctx, upgradedAccount)
		if err = accountschema.Up(ctx, upgradedAccount, am); err != nil {
			t.Fatal("cumulative closure Account Up", err)
		}
		if _, err = migrations.VerifyApplied(ctx, upgradedAccount, am); err != nil {
			t.Fatal(err)
		}
		if accountData != stageAccountFacts(t, ctx, upgradedAccount) {
			t.Fatal("cumulative closure changed retained Account facts")
		}
		if stageShape(t, ctx, upgradedAccount) != stageShape(t, ctx, ac) {
			t.Fatal("Account fresh/cumulative schema ACL shape mismatch")
		}
		migrations.Unlock(ctx, upgradedAccount)
		upgradedBilling := f.database(t, ctx, "billing", "billing_schema_owner")
		var oldBilling []migrations.Migration
		for _, x := range bm {
			oldBilling = append(oldBilling, x)
		}
		if err = Up(ctx, upgradedBilling, oldBilling); err != nil {
			t.Fatal("predecessor Billing Up", err)
		}
		if _, err = upgradedBilling.Exec(ctx, stageBillingSeed); err != nil {
			t.Fatal("existing Billing retained data seed", err)
		}
		billingData := stageWalletFacts(t, ctx, upgradedBilling)
		if err = Up(ctx, upgradedBilling, bm); err != nil {
			t.Fatal("cumulative closure Billing Up", err)
		}
		if _, err = migrations.VerifyApplied(ctx, upgradedBilling, bm); err != nil {
			t.Fatal(err)
		}
		if billingData != stageWalletFacts(t, ctx, upgradedBilling) {
			t.Fatal("cumulative closure changed retained Billing facts")
		}
		if stageShape(t, ctx, upgradedBilling) != stageShape(t, ctx, bc) {
			t.Fatal("Billing fresh/cumulative schema ACL shape mismatch")
		}
		migrations.Unlock(ctx, upgradedBilling)

	})
	t.Run("durable-marker-runtime-cannot-erase-and-public-rollback-rejected", func(t *testing.T) {
		var protected bool
		if err = tc.QueryRow(ctx, `SELECT pg_get_userbyid(c.relowner)='token_schema_owner' AND has_table_privilege('token_gateway',c.oid,'SELECT') AND has_table_privilege('token_gateway',c.oid,'INSERT') AND NOT has_table_privilege('token_gateway',c.oid,'UPDATE') AND NOT has_table_privilege('token_gateway',c.oid,'DELETE') FROM pg_class c WHERE c.oid='usage_replay_anonymized_users'::regclass`).Scan(&protected); err != nil || !protected {
			t.Fatal("marker owner/ACL not permanent", err)
		}
		ledger := stageValue(t, ctx, tc, `SELECT jsonb_agg(jsonb_build_array(version,checksum) ORDER BY version)::text FROM schema_migrations`)
		if err = migrations.DownOne(ctx, tc, tm); err == nil {
			t.Fatal("Token privacy marker rollback allowed")
		}
		if ledger != stageValue(t, ctx, tc, `SELECT jsonb_agg(jsonb_build_array(version,checksum) ORDER BY version)::text FROM schema_migrations`) {
			t.Fatal("rejected rollback changed Token ledger")
		}
		ledger = stageValue(t, ctx, ac, `SELECT jsonb_agg(jsonb_build_array(version,checksum) ORDER BY version)::text FROM schema_migrations`)
		if err = migrations.DownOne(ctx, ac, am); err == nil {
			t.Fatal("Account durable ACK rollback allowed")
		}
		if ledger != stageValue(t, ctx, ac, `SELECT jsonb_agg(jsonb_build_array(version,checksum) ORDER BY version)::text FROM schema_migrations`) {
			t.Fatal("rejected rollback changed Account ledger")
		}
	})
	args := []string{"test", "-mod=readonly", "-tags=nomsgpack", "-count=1", "-json", "./internal/account", "-run", "^TestClosureClaimACKOrder$"}
	if runtime.GOOS != "windows" {
		args = append(args, "-race")
	}
	child := exec.CommandContext(ctx, os.Getenv("F08_TEST_GO"), args...)
	child.Dir = "../.."
	child.Env = append(os.Environ(), "F08_ACCOUNT_URL="+f.serviceURL(t, ac, "account_service"), "F08_BILLING_URL="+f.serviceURL(t, bc, "billing_service"), "F08_TOKEN_URL="+f.serviceURL(t, tc, "token_gateway"))
	out, e := child.CombinedOutput()
	if err = os.WriteFile(os.Getenv("F08_TEST_CHILD_LOG"), out, 0600); err != nil {
		t.Fatal(err)
	}
	if e != nil {
		t.Fatal("actual closure child failed; immutable log retained")
	}
}
