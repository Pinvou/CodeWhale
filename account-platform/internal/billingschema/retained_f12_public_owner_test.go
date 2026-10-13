package billingschema

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestPrivateF12PublicOwnerIntegration(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("PINVOU_TEST_SCHEMA_DATABASE_URL not configured; no PG validation claimed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	admin, e := pgx.Connect(ctx, raw)
	if e != nil {
		t.Fatal("isolated connect")
	}
	// The URL must explicitly select an isolated superuser session with no
	// existing deployment actors; original UUID/cleanup guards remain below.
	var actors int
	var isolatedOwner bool
	e = admin.QueryRow(ctx, `SELECT current_user=session_user AND (SELECT rolsuper FROM pg_roles WHERE rolname=session_user),
 (SELECT count(*) FROM pg_roles WHERE rolname !~ '^pg_' AND rolname<>session_user)`).Scan(&isolatedOwner, &actors)
	_ = admin.Close(ctx)
	if e != nil || !isolatedOwner || actors != 0 {
		t.Fatal("explicit isolated owner and exclusive actor guard")
	}
	f := newStageFixture(t, ctx, raw)
	t.Run("public-account-original073-policy", func(t *testing.T) {
		c := f.database(t, ctx, "account", "account_service")
		all, e := migrations.Load("../../migrations/account")
		if e != nil {
			t.Fatal(e)
		}
		if e = accountschema.Up(ctx, c, all); e != nil {
			t.Fatal("public Account Up", e)
		}
		if _, e = migrations.VerifyApplied(ctx, c, all); e != nil {
			t.Fatal(e)
		}
		if digest, err := approvalpolicy.VerifyPolicySet(ctx, c); err != nil || len(digest) != 64 {
			t.Fatal("actual full current 26 policies differ", err)
		}

		migrations.Unlock(ctx, c)
		var n int
		if e = c.QueryRow(ctx, `SELECT count(*) FROM approval_command_policies WHERE command_type='PAYMENT_FINANCIAL_REPAIR' AND policy_version=1 AND snapshot_schema_version=1`).Scan(&n); e != nil || n != 1 {
			t.Fatal("original policy", n, e)
		}
	})

	t.Run("fresh-and-parent079-upgrade-acl", func(t *testing.T) {
		all, e := migrations.Load("../../migrations/billing")
		if e != nil {
			t.Fatal(e)
		}
		for _, upgrade := range []bool{false, true} {
			c := f.database(t, ctx, "billing", "billing_schema_owner")
			if _, e = c.Exec(ctx, "SET ROLE billing_service"); e != nil {
				t.Fatal(e)
			}
			before79 := ""
			if upgrade {
				parent := slices.DeleteFunc(slices.Clone(all), func(m migrations.Migration) bool {
					return m.Version == 65 || m.Version == 66 || m.Version == 77 || m.Version == 900016 || m.Version == 900017
				})
				// Preserve the complete inherited F11 prefix, excluding all five
				// F12 Billing identities (approval columns/allowlist/requester/lock/lookup).
				// In particular, the lock capability must accompany its original columns.
				if e = Up(ctx, c, parent); e != nil {
					t.Fatal("inherited pre-F12 SQL install", e)
				}
				before79 = stageValue(t, ctx, c, "SELECT checksum FROM schema_migrations WHERE version=79")
			}
			if e = Up(ctx, c, all); e != nil {
				t.Fatal("current public fresh/upgrade", upgrade, e)
			}
			if _, e = migrations.VerifyApplied(ctx, c, all); e != nil {
				t.Fatal(e)
			}
			if stageValue(t, ctx, c, "SELECT has_schema_privilege('billing_service','public','CREATE')::text") != "false" {
				t.Fatal("runtime CREATE retained")
			}
			if stageValue(t, ctx, c, "SELECT session_user||':'||current_user") != "billing_migrator:billing_service" {
				t.Fatal("migration identity changed")
			}
			if upgrade && stageValue(t, ctx, c, "SELECT checksum FROM schema_migrations WHERE version=79") != before79 {
				t.Fatal("already installed 079 rewritten")
			}
			if stageValue(t, ctx, c, "SELECT count(*)::text FROM schema_migrations WHERE version IN(65,66,77,79)") != "4" {
				t.Fatal("missing lower-number repair migrations")
			}
			migrations.Unlock(ctx, c)
		}
	})
	t.Run("original079-loan-rollback-and-pins", func(t *testing.T) {
		all, e := migrations.Load("../../migrations/billing")
		if e != nil {
			t.Fatal(e)
		}
		c := f.database(t, ctx, "billing", "billing_schema_owner")
		if _, e = c.Exec(ctx, "SET ROLE billing_service"); e != nil {
			t.Fatal(e)
		}
		before := slices.DeleteFunc(slices.Clone(all), func(m migrations.Migration) bool { return m.Version == 79 })
		if e = Up(ctx, c, before); e != nil {
			t.Fatal(e)
		}
		ordered, e := Ordered(all)
		if e != nil {
			t.Fatal(e)
		}
		injected := errors.New("owned fixture after-original079 injection")
		e = migrations.UpWithHooks(ctx, c, ordered, migrations.UpHooks{BeforeApply: prepareBillingMigration, ApplySQL: applyBillingMigration, LedgerRole: "billing_schema_owner", AfterApply: func(ctx context.Context, tx pgx.Tx, m migrations.Migration) error {
			if m.Version != 79 {
				return finishBillingMigration(ctx, tx, m)
			}
			var create bool
			if e := tx.QueryRow(ctx, "SELECT has_schema_privilege('billing_service','public','CREATE')").Scan(&create); e != nil {
				return e
			}
			if !create {
				return errors.New("loan absent before originalDDL completion")
			}
			if e := finishBillingMigration(ctx, tx, m); e != nil {
				return e
			}
			return injected
		}})
		if e == nil || !strings.Contains(e.Error(), injected.Error()) {
			t.Fatal("injection not reached", e)
		}
		if stageValue(t, ctx, c, "SELECT has_schema_privilege('billing_service','public','CREATE')::text") != "false" {
			t.Fatal("loan survived rollback")
		}
		if stageValue(t, ctx, c, "SELECT (to_regclass('registration_bonus_policy_state') IS NULL)::text") != "true" || stageValue(t, ctx, c, "SELECT count(*)::text FROM schema_migrations WHERE version=79") != "0" {
			t.Fatal("failed originalDDL/ledger not atomic")
		}
		for _, m := range all {
			if m.Version != 79 {
				continue
			}
			for _, mutate := range []func(*migrations.Migration){func(x *migrations.Migration) { x.Name = "foreign" }, func(x *migrations.Migration) { x.UpSQL += " " }, func(x *migrations.Migration) { x.DownSQL += " " }} {
				bad := m
				mutate(&bad)
				if e := protectBillingMigrationAndLendBonusDDL(ctx, nil, bad); e == nil {
					t.Fatal("foreign079 pin accepted")
				}
			}
		}
		if e = Up(ctx, c, all); e != nil {
			t.Fatal("after rollback original install", e)
		}
		migrations.Unlock(ctx, c)
	})
	t.Run("original-owner-contract-child", func(t *testing.T) {
		dir := "../billing"
		args := []string{"test", "-mod=readonly", "-race", "-count=1", "-json", ".", "-run", "^(TestPaymentFinancialRepairPostgresIntegration|TestRetainedPaymentRepairProviderFacts|TestPaymentFinancialRepairFrozenExecutionPostgres)$"}
		binary := os.Getenv("F12_GO_BINARY")
		if binary == "" {
			var err error
			binary, err = exec.LookPath("go")
			if err != nil {
				t.Fatal("enabled PostgreSQL fixture requires Go")
			}
		}
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "PINVOU_TEST_BILLING_DATABASE_URL="+raw)
		out, e := cmd.CombinedOutput()
		dest := os.Getenv("F12_CHILD_LOG")
		if dest == "" {
			dest = filepath.Join(t.TempDir(), "f12-owner-child.jsonl")
		}
		if er := os.WriteFile(dest, out, 0600); er != nil {
			t.Fatal(er)
		}
		if e != nil {
			t.Fatal("original owner child failed; retained log", e)
		}
		pass, failed, skipped := 0, 0, 0
		original := make(map[string]bool)
		for _, line := range strings.Split(string(out), "\n") {
			var a struct{ Action, Test string }
			if json.Unmarshal([]byte(line), &a) == nil && a.Action == "pass" && a.Test != "" {
				pass++
				original[a.Test] = true
			}
			if a.Test != "" && a.Action == "fail" {
				failed++
			}
			if a.Test != "" && a.Action == "skip" {
				skipped++
			}
		}
		if pass == 0 || failed != 0 || skipped != 0 || !original["TestPaymentFinancialRepairPostgresIntegration"] || !original["TestRetainedPaymentRepairProviderFacts"] || !original["TestPaymentFinancialRepairFrozenExecutionPostgres"] {
			t.Fatal("zero original owner assertions")
		}
	})
}
