package billingschema

import (
	"context"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
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
		if err := migrations.DownOne(ctx, connection, installed); err == nil {
			t.Fatal("first-delivery metadata rollback unexpectedly succeeded")
		}
		if _, err := migrations.VerifyApplied(ctx, connection, installed); err != nil {
			t.Fatalf("failed rollback changed the migration ledger: %v", err)
		}
	})
}
