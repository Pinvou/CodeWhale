package billingschema

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/tokenschema"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
		// The 900006 rollback contract belongs to its historical delivery. Keep
		// the full current Account installation above for the lifecycle checks;
		// newer forward-only migrations must not be weakened to reach this one.
		ac := f.database(t, ctx, "account", "account_service")
		var historicalAccount []migrations.Migration
		for _, migration := range am {
			if migration.Version <= 900006 {
				historicalAccount = append(historicalAccount, migration)
			}
		}
		if err := accountschema.Up(ctx, ac, historicalAccount); err != nil {
			t.Fatal("historical Account public Up", err)
		}
		if _, err := migrations.VerifyApplied(ctx, ac, historicalAccount); err != nil {
			t.Fatal("historical Account history", err)
		}
		am := historicalAccount
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
		// Only unused 900006 can roll back; 900005 must retain cleanup ACKs.
		var latest int64
		var incompatibleBindings int
		if err = ac.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&latest); err != nil || latest != 900006 {
			t.Fatal("expected role CHECK before cleanup ACK rollback", latest, err)
		}
		if err = ac.QueryRow(ctx, `SELECT count(*) FROM role_bindings WHERE role='ORG_MODEL_ADMIN'`).Scan(&incompatibleBindings); err != nil || incompatibleBindings != 0 {
			t.Fatal("safe role CHECK rollback requires no incompatible history", incompatibleBindings, err)
		}
		accountLedger := func(withoutRoleCheck bool) string {
			t.Helper()
			var facts string
			if err := ac.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(entry) ORDER BY version)::text FROM schema_migrations entry WHERE NOT $1::boolean OR version<>900006`, withoutRoleCheck).Scan(&facts); err != nil {
				t.Fatal(err)
			}
			return facts
		}
		accountFacts := func() string {
			t.Helper()
			return stageValue(t, ctx, ac, `SELECT jsonb_build_object(
'users',(SELECT jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text) FROM users f),
'identities',(SELECT jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text) FROM identities f),
'user_sessions',(SELECT jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text) FROM user_sessions f),
'memberships',(SELECT jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text) FROM memberships f),
'membership_units',(SELECT jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text) FROM membership_units f),
'invitations',(SELECT jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text) FROM invitations f),
'account_closure_requests',(SELECT jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text) FROM account_closure_requests f),
'account_closure_commands',(SELECT jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text) FROM account_closure_commands f),
'account_closure_audit',(SELECT jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text) FROM account_closure_audit f),
'account_subject_retention_records',(SELECT jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text) FROM account_subject_retention_records f),
'roles',(SELECT jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text) FROM roles f),
'permissions',(SELECT jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text) FROM permissions f),
'role_permissions',(SELECT jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text) FROM role_permissions f),
'role_bindings',(SELECT jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text) FROM role_bindings f),
'role_assignments',(SELECT jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text) FROM role_assignments f),
'authorization_versions',(SELECT jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text) FROM authorization_versions f),
'outbox_events',(SELECT jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text) FROM outbox_events f))::text`)
		}
		beforeFacts, expectedRemainingLedger := accountFacts(), accountLedger(true)
		beforeShape := stageShape(t, ctx, ac)
		roleCheckIdentity := stageValue(t, ctx, ac, `SELECT (to_jsonb(entry)-'applied_at')::text FROM schema_migrations entry WHERE version=900006`)
		if err = migrations.DownOne(ctx, ac, am); err != nil {
			t.Fatal("unused role CHECK did not roll back safely", err)
		}
		var remaining []migrations.Migration
		for _, migration := range am {
			if migration.Version != 900006 {
				remaining = append(remaining, migration)
			}
		}
		if len(remaining) != len(am)-1 || accountLedger(false) != expectedRemainingLedger || accountFacts() != beforeFacts {
			t.Fatal("safe role CHECK rollback changed retained facts or unrelated ledger")
		}
		if _, err = migrations.VerifyApplied(ctx, ac, remaining); err != nil {
			t.Fatal("safe rollback left unexpected Account history", err)
		}
		if err = ac.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&latest); err != nil || latest != 900005 {
			t.Fatal("cleanup ACK guard is not the next public rollback", latest, err)
		}
		err = migrations.DownOne(ctx, ac, remaining)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55000" || pgErr.Message != "account closure cleanup acknowledgements are forward-only" {
			t.Fatal("actual Account cleanup ACK rollback was not rejected", err)
		}
		if accountLedger(false) != expectedRemainingLedger || accountFacts() != beforeFacts {
			t.Fatal("rejected cleanup ACK rollback changed Account facts or full remaining ledger")
		}
		if _, err = migrations.VerifyApplied(ctx, ac, remaining); err != nil {
			t.Fatal("rejected rollback changed remaining Account history", err)
		}
		// Restore through public Up before the real ACK child. Only the re-applied
		// 900006 ledger row may receive a new applied_at; no history is rewritten.
		if err = accountschema.Up(ctx, ac, am); err != nil {
			t.Fatal("restore full Account schema before ACK child", err)
		}
		if _, err = migrations.VerifyApplied(ctx, ac, am); err != nil {
			t.Fatal("restored Account history before ACK child", err)
		}
		if accountLedger(true) != expectedRemainingLedger || accountFacts() != beforeFacts || stageShape(t, ctx, ac) != beforeShape || stageValue(t, ctx, ac, `SELECT (to_jsonb(entry)-'applied_at')::text FROM schema_migrations entry WHERE version=900006`) != roleCheckIdentity {
			t.Fatal("public restore changed retained facts, schema or migration identity")
		}
		migrations.Unlock(ctx, ac)
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
