package approvalpolicy

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestEntitlementCorrectionPolicyV2PostgresIntegration proves the Account
// command-policy metadata that accompanies the entitlement correction domain
// gate: the immutable v1 row stays in place, the latest version is the v2
// closed domain snapshot schema, and the command stays non-executable.
func TestEntitlementCorrectionPolicyV2PostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_ACCOUNT_DATABASE_URL is not configured")
	}
	if testing.Short() && os.Getenv("PINVOU_TEST_ACCOUNT_POLICY_GATE") != "1" {
		t.Skip("Account command-policy PostgreSQL gate is run explicitly before the short race suite")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	connection, cleanup := newEntitlementPolicyFixtureDatabase(t, ctx, databaseURL)
	t.Cleanup(cleanup)
	loaded, err := migrations.Load("../../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrations.Up(ctx, connection, loaded); err != nil {
		t.Fatalf("apply Account migrations: %v", err)
	}
	migrations.Unlock(ctx, connection)
	if _, err := connection.Exec(ctx, `RESET ROLE; RESET SESSION AUTHORIZATION`); err != nil {
		t.Fatal(err)
	}

	expected, err := Lookup("ENTITLEMENT_COMPONENT_ADJUSTMENT")
	if err != nil {
		t.Fatal(err)
	}
	if expected.PolicyVersion != 2 || expected.SnapshotSchemaVersion != 2 {
		t.Fatalf("registry correction policy=%d/%d, want 2/2", expected.PolicyVersion, expected.SnapshotSchemaVersion)
	}
	if Executable("ENTITLEMENT_COMPONENT_ADJUSTMENT") {
		t.Fatal("the correction command must stay outside the executable set")
	}
	var actual Policy
	var mode string
	var critical []byte
	var expiresSeconds, executionSeconds int64
	if err := connection.QueryRow(ctx, `SELECT command_type,policy_version,approval_mode,requester_permission,COALESCE(approver_permission,''),risk_class,approval_count,self_approval_forbidden,snapshot_schema_version,critical_resource_fields,EXTRACT(EPOCH FROM expires_after)::bigint,EXTRACT(EPOCH FROM execution_window)::bigint,executor_identity FROM approval_command_policies WHERE command_type='ENTITLEMENT_COMPONENT_ADJUSTMENT' AND policy_version=2`).Scan(
		&actual.CommandType, &actual.PolicyVersion, &mode, &actual.RequesterPermission, &actual.ApproverPermission, &actual.RiskClass, &actual.ApprovalCount, &actual.SelfApprovalForbidden, &actual.SnapshotSchemaVersion, &critical, &expiresSeconds, &executionSeconds, &actual.ExecutorIdentity); err != nil {
		t.Fatal(err)
	}
	actual.Mode = Mode(mode)
	actual.ExpiresAfter = time.Duration(expiresSeconds) * time.Second
	actual.ExecutionWindow = time.Duration(executionSeconds) * time.Second
	if err := json.Unmarshal(critical, &actual.CriticalResourceFields); err != nil {
		t.Fatal(err)
	}
	if !policiesEqual(expected, actual) {
		t.Fatalf("database and registry correction policy differ:\nexpected=%#v\nactual=%#v", expected, actual)
	}
	var legacyRows int
	if err := connection.QueryRow(ctx, `SELECT count(*) FROM approval_command_policies WHERE command_type='ENTITLEMENT_COMPONENT_ADJUSTMENT' AND policy_version=1 AND snapshot_schema_version=1 AND approval_mode='APPROVAL_REQUIRED'`).Scan(&legacyRows); err != nil || legacyRows != 1 {
		t.Fatalf("immutable v1 policy row must remain: count=%d err=%v", legacyRows, err)
	}
	if _, err := connection.Exec(ctx, `UPDATE approval_command_policies SET risk_class='CHANGED' WHERE command_type='ENTITLEMENT_COMPONENT_ADJUSTMENT' AND policy_version=2`); err == nil {
		t.Fatal("installed v2 policy must be immutable")
	}
}

// newEntitlementPolicyFixtureDatabase provisions a dedicated Account database
// that mirrors the production role topology: the runtime role owns the
// database and migrations run through the dedicated migrator identity.
func newEntitlementPolicyFixtureDatabase(t *testing.T, ctx context.Context, databaseURL string) (*pgx.Conn, func()) {
	t.Helper()
	base, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := pgx.ConnectConfig(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.Exec(ctx, `
DO $$ BEGIN
  IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='account_approval_authorization_owner') THEN
    CREATE ROLE account_approval_authorization_owner NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
  END IF;
  IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='account_approval_writer') THEN
    CREATE ROLE account_approval_writer LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
  END IF;
  IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='account_approval_backfill') THEN
    CREATE ROLE account_approval_backfill LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
  END IF;
  IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='account_approval_recovery') THEN
    CREATE ROLE account_approval_recovery NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
  END IF;
END $$;
GRANT account_approval_authorization_owner TO account_migrator WITH INHERIT FALSE, SET TRUE, ADMIN TRUE;`); err != nil {
		t.Fatal(err)
	}
	name := "account_policy_fixture_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err := bootstrap.Exec(ctx, `CREATE DATABASE `+identifier+` OWNER account_service TEMPLATE template0 ENCODING 'UTF8'`); err != nil {
		t.Fatal(err)
	}
	configuration := base.Copy()
	configuration.Database = name
	connection, err := pgx.ConnectConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `GRANT CREATE ON DATABASE `+identifier+` TO account_migrator; SET SESSION AUTHORIZATION account_migrator; SET ROLE account_service`); err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = connection.Exec(cleanupCtx, `RESET ROLE; RESET SESSION AUTHORIZATION`)
		_ = connection.Close(context.Background())
		if _, cleanupErr := bootstrap.Exec(cleanupCtx, `DROP DATABASE `+identifier); cleanupErr != nil {
			t.Errorf("remove isolated Account policy database: %v", cleanupErr)
		}
		_ = bootstrap.Close(context.Background())
	}
	return connection, cleanup
}
