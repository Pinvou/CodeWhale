package billingschema

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"os"
	"testing"
	"time"
)

// Account installation uses the existing cross-service fixture because its
// frozen public Outbox contract requires real deployment actors and owned DBs.
func TestInstalledPersonalAccountSchemaPostgres(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL")
	if raw == "" {
		t.Skip("PINVOU_TEST_ACCOUNT_DATABASE_URL is required for actual PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	connection := f.database(t, ctx, "account", "account_service")
	installed, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if err := accountschema.Up(ctx, connection, installed); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)
	runtime, err := pgx.Connect(ctx, f.serviceURL(t, connection, "account_service"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.Close(context.Background()) })
	var actor, owner string
	var privileged bool
	if err = runtime.QueryRow(ctx, `SELECT session_user,current_user,(SELECT rolsuper OR rolcreaterole OR rolcreatedb OR rolbypassrls FROM pg_roles WHERE rolname=current_user)`).Scan(&actor, &owner, &privileged); err != nil || actor != "account_service" || owner != actor || privileged {
		t.Fatal("runtime identity is not the real unprivileged Account actor", err)
	}
	if _, err := migrations.VerifyApplied(ctx, runtime, installed); err != nil {
		t.Fatal("runtime ledger verification", err)
	}
	verification, err := migrations.VerifyApplied(ctx, connection, installed)
	var latest int64
	for _, migration := range installed {
		if migration.Version > latest {
			latest = migration.Version
		}
	}
	if err != nil || verification.AppliedCount != len(installed) || verification.LatestVersion != latest {
		t.Fatalf("installed migration verification: %+v err=%v", verification, err)
	}
	t.Run("mature-personal-and-sms-authority", func(t *testing.T) {
		var scope, functionName string
		if err := runtime.QueryRow(ctx, `SELECT scope,to_regprocedure('increment_authorization_scope_version(uuid,text,uuid)')::text FROM roles WHERE code='PERSONAL_USER'`).Scan(&scope, &functionName); err != nil || scope != "PERSONAL" || functionName == "" {
			t.Fatalf("personal authority not installed: scope=%q helper=%q err=%v", scope, functionName, err)
		}
		var smsTable string
		if err := runtime.QueryRow(ctx, `SELECT to_regclass('sms_configuration_versions')::text`).Scan(&smsTable); err != nil || smsTable == "" {
			t.Fatalf("versioned SMS configuration not installed: %q err=%v", smsTable, err)
		}
	})
	t.Run("repeat-preserves-ledger-and-role-edges", func(t *testing.T) {
		var before string
		if err := connection.QueryRow(ctx, `SELECT md5(string_agg(role_code||':'||permission_code,',' ORDER BY role_code,permission_code)) FROM role_permissions`).Scan(&before); err != nil {
			t.Fatal(err)
		}
		if err := accountschema.Up(ctx, connection, installed); err != nil {
			t.Fatal(err)
		}
		var after string
		if err := connection.QueryRow(ctx, `SELECT md5(string_agg(role_code||':'||permission_code,',' ORDER BY role_code,permission_code)) FROM role_permissions`).Scan(&after); err != nil || before != after {
			t.Fatalf("repeat changed role edges: err=%v", err)
		}
		if _, err := migrations.VerifyApplied(ctx, connection, installed); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("forward-only-rollback-is-atomic", func(t *testing.T) {
		assertCurrentAccount900013Down(t, ctx, connection, installed)
	})
	t.Run("historical-900006-down-900005-guard-and-restore", func(t *testing.T) {
		assertHistoricalAccount900006Down(t, ctx, f, installed)
	})
}

// These helpers cover only the two fixed Account rollback contracts. Historical
// seeds are schema fixtures, not a claim that modern repository producers ran.
func accountRollbackFacts(t *testing.T, ctx context.Context, c *pgx.Conn) string {
	t.Helper()
	return stageValue(t, ctx, c, `SELECT jsonb_build_object(
 'users',(SELECT jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text) FROM users f),
 'roles',(SELECT jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text) FROM roles f),
 'permissions',(SELECT jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text) FROM permissions f),
 'role_permissions',(SELECT jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text) FROM role_permissions f),
 'role_bindings',(SELECT jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text) FROM role_bindings f),
 'role_assignments',(SELECT jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text) FROM role_assignments f),
 'authorization_versions',(SELECT jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text) FROM authorization_versions f),
 'memberships',(SELECT jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text) FROM memberships f),
 'organizations',(SELECT jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text) FROM organizations f),
 'organization_units',(SELECT jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text) FROM organization_units f),
 'membership_units',(SELECT jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text) FROM membership_units f),
 'registration_operations',(SELECT jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text) FROM registration_operations f),
 'wallet_provisioning_jobs',(SELECT jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text) FROM wallet_provisioning_jobs f),
 'eligibility',(SELECT jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text) FROM registration_bonus_mobile_eligibility f))::text`)
}
func accountRollbackLedger(t *testing.T, ctx context.Context, c *pgx.Conn, withoutRole bool) string {
	t.Helper()
	query := `SELECT jsonb_agg(to_jsonb(f) ORDER BY version)::text FROM schema_migrations f`
	if withoutRole {
		query += ` WHERE version<>900006`
	}
	return stageValue(t, ctx, c, query)
}
func assertCurrentAccount900013Down(t *testing.T, ctx context.Context, c *pgx.Conn, all []migrations.Migration) {
	t.Helper()
	if stageValue(t, ctx, c, `SELECT version::text||':'||name FROM schema_migrations ORDER BY version DESC LIMIT 1`) != "900013:model_lifecycle_force_cancel_policy" {
		t.Fatal("current rollback target changed")
	}
	ledger, facts, shape := accountRollbackLedger(t, ctx, c, false), accountRollbackFacts(t, ctx, c), stageShape(t, ctx, c)
	var e *pgconn.PgError
	if err := migrations.DownOne(ctx, c, all); !errors.As(err, &e) || e.Code != "P0001" || e.Message != "lifecycle policy v3 is forward-only; enter SAFE_ROLLBACK without deleting approval history" {
		t.Fatal("current lifecycle rollback must hit exact guard", err)
	}
	if accountRollbackLedger(t, ctx, c, false) != ledger || accountRollbackFacts(t, ctx, c) != facts || stageShape(t, ctx, c) != shape {
		t.Fatal("rejected current rollback changed history, facts or schema")
	}
	if _, err := migrations.VerifyApplied(ctx, c, all); err != nil {
		t.Fatal(err)
	}
}
func assertHistoricalAccount900006Down(t *testing.T, ctx context.Context, f *stageFixture, all []migrations.Migration) {
	t.Helper()
	var historical []migrations.Migration
	h := sha256.New()
	for _, m := range all {
		if m.Version <= 900006 {
			historical = append(historical, m)
			fmt.Fprintf(h, "%d:%s:%s\n", m.Version, m.Name, sqlChecksum(m.UpSQL))
		}
	}
	// Pin version/name/canonical SQL identity of this contract prerequisite set,
	// not an assertion that all lower-numbered SQL shipped on the same date.
	if fmt.Sprintf("%x", h.Sum(nil)) != "1f632e0158abd496991c1c1390c937729160902bd0ae64eb197383336ec21b85" {
		t.Fatal("historical rollback prerequisite identities changed")
	}
	c := f.database(t, ctx, "account", "account_service")
	if err := migrations.Up(ctx, c, selectVersions(historical, []int64{1, 2, 3, 4, 6, 7, 8, 9, 10, 11, 12, 13, 14, 16, 17})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, stageAccountSeed); err != nil {
		t.Fatal(err)
	}
	if err := accountschema.Up(ctx, c, historical); err != nil {
		t.Fatal("historical prerequisite installation", err)
	}
	if _, err := migrations.VerifyApplied(ctx, c, historical); err != nil {
		t.Fatal(err)
	}
	// Nonempty original membership attachment and registration-input fixtures.
	if _, err := c.Exec(ctx, `INSERT INTO organizations(id,name,status,created_by,created_at,updated_at) VALUES('a1111111-1111-4111-8111-111111111111','historical','ACTIVE','11111111-1111-4111-8111-111111111111',now(),now());
 INSERT INTO organization_units(id,organization_id,name,status,created_at,updated_at) VALUES('a2222222-2222-4222-8222-222222222222','a1111111-1111-4111-8111-111111111111','historical','ACTIVE',now(),now());
 INSERT INTO memberships(id,organization_id,user_id,status,joined_at,updated_at) VALUES('a3333333-3333-4333-8333-333333333333','a1111111-1111-4111-8111-111111111111','11111111-1111-4111-8111-111111111111','ACTIVE',now(),now());
 INSERT INTO role_bindings(id,membership_id,role,created_by,created_at,starts_at) VALUES('a7777777-7777-4777-8777-777777777777','a3333333-3333-4333-8333-333333333333','OWNER','11111111-1111-4111-8111-111111111111',now(),now());
 INSERT INTO membership_units(membership_id,unit_id,created_at) VALUES('a3333333-3333-4333-8333-333333333333','a2222222-2222-4222-8222-222222222222',now());
 INSERT INTO sms_challenges(id,mobile_e164,purpose,status,expires_at,created_at,updated_at,provider_id,adapter_version) VALUES('a4444444-4444-4444-8444-444444444444','+8613800000000','REGISTER','EXPIRED',now(),now(),now(),'fixture','v1');
 INSERT INTO registration_operations(id,candidate_user_id,query_token_hash,request_fingerprint,username_normalized,mobile_e164,sms_challenge_id,password_policy_version,committed_user_id,status,next_retry_at,created_at,updated_at) VALUES('a5555555-5555-4555-8555-555555555555','a6666666-6666-4666-8666-666666666666',decode(repeat('11',32),'hex'),decode(repeat('22',32),'hex'),'historical-fixture','+8613800000000','a4444444-4444-4444-8444-444444444444','v1','11111111-1111-4111-8111-111111111111','EXPIRED',now(),now(),now());`); err != nil {
		t.Fatal(err)
	}
	if stageValue(t, ctx, c, `SELECT count(*)::text FROM role_bindings WHERE role='ORG_MODEL_ADMIN'`) != "0" {
		t.Fatal("incompatible historical binding")
	}
	facts, shape, remainingLedger := accountRollbackFacts(t, ctx, c), stageShape(t, ctx, c), accountRollbackLedger(t, ctx, c, true)
	identity := stageValue(t, ctx, c, `SELECT (to_jsonb(f)-'applied_at')::text FROM schema_migrations f WHERE version=900006`)
	if err := migrations.DownOne(ctx, c, historical); err != nil {
		t.Fatal("unused role CHECK rollback", err)
	}
	if stageValue(t, ctx, c, `SELECT (position('ORG_MODEL_ADMIN' IN pg_get_constraintdef(oid))=0)::text FROM pg_constraint WHERE conrelid='role_bindings'::regclass AND conname='role_bindings_role_check'`) != "true" {
		t.Fatal("historical role CHECK did not shrink")
	}
	var remaining []migrations.Migration
	for _, m := range historical {
		if m.Version != 900006 {
			remaining = append(remaining, m)
		}
	}
	if len(remaining) != len(historical)-1 || accountRollbackLedger(t, ctx, c, false) != remainingLedger || accountRollbackFacts(t, ctx, c) != facts {
		t.Fatal("900006 rollback changed retained history or facts")
	}
	if _, err := migrations.VerifyApplied(ctx, c, remaining); err != nil {
		t.Fatal(err)
	}
	if stageValue(t, ctx, c, `SELECT max(version)::text FROM schema_migrations`) != "900005" {
		t.Fatal("wrong next historical target")
	}
	afterDownShape := stageShape(t, ctx, c)
	var e *pgconn.PgError
	if err := migrations.DownOne(ctx, c, remaining); !errors.As(err, &e) || e.Code != "55000" || e.Message != "account closure cleanup acknowledgements are forward-only" {
		t.Fatal("historical cleanup guard", err)
	}
	if accountRollbackLedger(t, ctx, c, false) != remainingLedger || accountRollbackFacts(t, ctx, c) != facts || stageShape(t, ctx, c) != afterDownShape {
		t.Fatal("rejected historical rollback changed facts")
	}
	if _, err := migrations.VerifyApplied(ctx, c, remaining); err != nil {
		t.Fatal(err)
	}
	if err := accountschema.Up(ctx, c, historical); err != nil {
		t.Fatal(err)
	}
	if _, err := migrations.VerifyApplied(ctx, c, historical); err != nil {
		t.Fatal(err)
	}
	if accountRollbackLedger(t, ctx, c, true) != remainingLedger || accountRollbackFacts(t, ctx, c) != facts || stageShape(t, ctx, c) != shape || stageValue(t, ctx, c, `SELECT (to_jsonb(f)-'applied_at')::text FROM schema_migrations f WHERE version=900006`) != identity {
		t.Fatal("historical restore changed original facts or identity")
	}
	migrations.Unlock(ctx, c)
}
