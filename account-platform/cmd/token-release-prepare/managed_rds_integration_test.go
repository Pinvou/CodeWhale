package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/postgrescontrol"
	"github.com/Pinvou/pinvou3/account-platform/internal/tokenschema"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestManagedRDSReleaseActualPostgres(t *testing.T) {
	testManagedRDSReleaseActualPostgres(t, false)
}

func TestManagedRDSReleaseProductionHistoryPostgres(t *testing.T) {
	if os.Getenv("PINVOU_TEST_LEGACY_MIGRATIONS_DIRECTORY") == "" {
		t.Skip("original published migration package is not configured")
	}
	testManagedRDSReleaseActualPostgres(t, true)
}

func testManagedRDSReleaseActualPostgres(t *testing.T, legacyUpgrade bool) {
	raw := os.Getenv("PINVOU_TEST_MANAGED_POSTGRES_URL")
	if raw == "" {
		t.Skip("dedicated TLS managed-control fixture is not configured")
	}
	config, err := pgx.ParseConfig(raw)
	if err != nil || config.Database != "managed_control_fixture" || config.User != "pinvou_control_fixture" || config.Host != "127.0.0.1" {
		t.Fatal("refusing a database outside the dedicated local managed-control fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	admin, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	var exclusive bool
	if admin.QueryRow(ctx, `SELECT current_setting('data_directory')='/var/lib/postgresql/data/managed_control_fixture'
 AND NOT EXISTS(SELECT 1 FROM pg_database WHERE NOT datistemplate AND datname NOT IN ('postgres','managed_control_fixture'))
 AND NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname !~ '^pg_' AND rolname<>session_user)`).Scan(&exclusive) != nil || !exclusive {
		t.Fatal("refusing writes outside the newly initialized exclusive fixture cluster")
	}
	roles := []string{"token_gateway", "token_schema_owner", "token_cap_policy_deployer", "token_migrator", "token_policy_operator", "pg_rds_superuser", "managed_dba_fixture", "aurora", "test_credential", "test_gateway", "retired_fixture_writer"}
	var exists bool
	if err := admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=ANY($1)) OR EXISTS(SELECT 1 FROM pg_database WHERE datname='rdsadmin')`, roles).Scan(&exists); err != nil || exists {
		t.Fatal("exclusive fixture roles/platform database must be absent")
	}
	database := "managed_token_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	var created []string
	var databaseCreated, platformCreated bool
	defer func() {
		clean, done := context.WithTimeout(context.Background(), 15*time.Second)
		defer done()
		if databaseCreated {
			var owner string
			var sessions int
			if err := admin.QueryRow(clean, `SELECT pg_get_userbyid(datdba),(SELECT count(*) FROM pg_stat_activity WHERE datname=$1) FROM pg_database WHERE datname=$1`, database).Scan(&owner, &sessions); err != nil || sessions != 0 || (owner != "token_gateway" && owner != "token_schema_owner") {
				t.Error("fixture database cleanup guard refused")
				return
			}
			if _, err := admin.Exec(clean, "DROP DATABASE "+pgx.Identifier{database}.Sanitize()); err != nil {
				t.Error("fixture database cleanup failed")
				return
			}
		}
		if platformCreated {
			var owner string
			if admin.QueryRow(clean, `SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname='rdsadmin'`).Scan(&owner) != nil || owner != "aurora" {
				t.Error("platform fixture owner changed; cleanup refused")
				return
			}
			if _, err := admin.Exec(clean, "DROP DATABASE rdsadmin"); err != nil {
				t.Error("platform fixture cleanup failed")
				return
			}
		}
		for i := len(created) - 1; i >= 0; i-- {
			if _, err := admin.Exec(clean, "DROP ROLE "+pgx.Identifier{created[i]}.Sanitize()); err != nil {
				t.Errorf("recorded fixture role cleanup failed: %s", created[i])
			}
		}
	}()
	exec := func(connection *pgx.Conn, sql string, args ...any) {
		t.Helper()
		if _, err := connection.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(admin, "CREATE ROLE token_gateway NOLOGIN INHERIT")
	created = append(created, "token_gateway")
	exec(admin, "CREATE DATABASE "+pgx.Identifier{database}.Sanitize()+" OWNER token_gateway")
	databaseCreated = true
	tokenConfig := config.Copy()
	tokenConfig.Database = database
	native, err := pgx.ConnectConfig(ctx, tokenConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { native.Close(context.Background()) }()
	request := releaseRequest{Operation: "prepare", Database: database, Approval: "fixture-approval", Maintenance: "fixture-exclusive-window", Candidates: []string{"test_credential", "test_gateway"}, Apply: true, Migrations: "../../migrations/token"}
	receipt, err := executeRelease(ctx, native, request)
	// Record exactly what the production preparation transaction committed.
	for _, role := range roles[1:5] {
		var present bool
		if admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)`, role).Scan(&present) == nil && present {
			created = append(created, role)
		}
	}
	for _, role := range request.Candidates {
		var present bool
		if admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)`, role).Scan(&present) == nil && present {
			created = append(created, role)
		}
	}
	if err != nil || !receipt.Applied {
		t.Fatalf("native preparation failed: %v", err)
	}
	exec(admin, "ALTER ROLE token_migrator LOGIN; ALTER ROLE token_policy_operator LOGIN")
	migratorConfig := tokenConfig.Copy()
	migratorConfig.User = "token_migrator"
	migrator, err := pgx.ConnectConfig(ctx, migratorConfig)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := migrations.Load("../../migrations/token")
	if err != nil {
		t.Fatal(err)
	}
	exec(migrator, "SET ROLE token_gateway")
	if legacyUpgrade {
		legacy, err := migrations.Load(filepath.Join(os.Getenv("PINVOU_TEST_LEGACY_MIGRATIONS_DIRECTORY"), "token"))
		if err != nil || len(legacy) != 34 || legacy[17].Version != 18 {
			t.Fatal("original production Token package is required")
		}
		// Original 029 revokes CREATE before original 033 creates an index.
		// Reproduce the shipped transaction-scoped DDL lease, keeping both SQL
		// files and original ledger identities unchanged. No schema is hand-
		// patched and no role receives committed runtime CREATE.
		err = migrations.UpWithHooks(ctx, migrator, legacy, migrations.UpHooks{
			LedgerRole: "token_schema_owner",
			BeforeApply: func(ctx context.Context, tx pgx.Tx, migration migrations.Migration) error {
				if migration.Version != 33 {
					return nil
				}
				_, err := tx.Exec(ctx, "SET ROLE token_schema_owner; GRANT CREATE ON SCHEMA public TO token_gateway; SET ROLE token_gateway")
				return err
			},
			AfterApply: func(ctx context.Context, tx pgx.Tx, migration migrations.Migration) error {
				if migration.Version != 33 {
					return nil
				}
				_, err := tx.Exec(ctx, "SET ROLE token_schema_owner; REVOKE CREATE ON SCHEMA public FROM token_gateway; SET ROLE token_gateway")
				return err
			},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	err = tokenschema.Up(ctx, migrator, loaded)
	migrations.Unlock(ctx, migrator)
	migrator.Close(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	exec(admin, "ALTER ROLE token_migrator NOLOGIN; ALTER ROLE token_policy_operator NOLOGIN")
	// Vanilla PostgreSQL reserves pg_ role creation even for superusers. Only
	// this physically isolated empty fixture emulates the provider-installed
	// catalog identity; all authority, memberships, ACLs and service migrations
	// below remain genuine PostgreSQL behavior. This is not an RDS engine test.
	exec(admin, "CREATE ROLE managed_platform_fixture NOLOGIN")
	exec(admin, "UPDATE pg_catalog.pg_authid SET rolname='pg_rds_superuser' WHERE rolname='managed_platform_fixture' AND NOT rolcanlogin AND NOT rolsuper")
	created = append(created, "pg_rds_superuser")
	for _, statement := range []string{"CREATE ROLE managed_dba_fixture LOGIN CREATEDB CREATEROLE BYPASSRLS", "CREATE ROLE aurora SUPERUSER NOLOGIN"} {
		exec(admin, statement)
		created = append(created, strings.Fields(statement)[2])
	}
	exec(admin, "GRANT pg_rds_superuser TO managed_dba_fixture")
	exec(admin, "GRANT token_schema_owner,token_gateway,token_cap_policy_deployer,test_credential,test_gateway TO managed_dba_fixture WITH ADMIN OPTION")
	// Provider-specific administration is simulated by explicit native grants;
	// this never grants authid SELECT or superuser to the managed login.
	exec(admin, "GRANT pg_read_all_stats TO managed_dba_fixture WITH ADMIN OPTION")
	exec(admin, "CREATE ROLE retired_fixture_writer LOGIN INHERIT; GRANT token_gateway TO retired_fixture_writer; GRANT retired_fixture_writer TO managed_dba_fixture WITH ADMIN OPTION")
	created = append(created, "retired_fixture_writer")
	exec(admin, "CREATE DATABASE rdsadmin OWNER aurora")
	platformCreated = true
	exec(admin, "REVOKE CONNECT ON DATABASE rdsadmin FROM PUBLIC")
	native.Close(context.Background())
	managedConfig := tokenConfig.Copy()
	managedConfig.User = "managed_dba_fixture"
	managed, err := pgx.ConnectConfig(ctx, managedConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer managed.Close(context.Background())
	if _, err := managed.Exec(ctx, "SELECT rolpassword FROM pg_authid"); err == nil {
		t.Fatal("managed test administrator unexpectedly has authid access")
	}
	t.Run("explicit-provider-and-authority-binding", func(t *testing.T) {
		inventory := releaseRequest{Operation: "inventory", Database: database, Migrations: request.Migrations}
		if _, err := executeRelease(ctx, managed, inventory); err == nil || err.Error() != "TOKEN_RELEASE_DBA_IDENTITY_REQUIRED" {
			t.Fatal("default path accepted a managed administrator")
		}
		inventory.Provider, inventory.ExpectedDBA, inventory.ExpectedHost = "aliyun-rds", managedConfig.User, managedConfig.Host
		if _, err := executeRelease(ctx, managed, inventory); err != nil {
			t.Fatal(err)
		}
		inventory.ExpectedHost = "wrong.example"
		if _, err := executeRelease(ctx, managed, inventory); err == nil {
			t.Fatal("wrong managed server accepted")
		}
		exec(admin, "REVOKE pg_rds_superuser FROM managed_dba_fixture")
		inventory.ExpectedHost = managedConfig.Host
		if _, err := executeRelease(ctx, managed, inventory); err == nil {
			t.Fatal("capabilities without platform authority accepted")
		}
		exec(admin, "GRANT pg_rds_superuser TO managed_dba_fixture")
	})
	t.Run("platform-boundary-rejects-connect-and-shared-acl", func(t *testing.T) {
		if err := rejectCrossDatabaseRoles(ctx, managed, request.Candidates, true); err != nil {
			t.Fatal(err)
		}
		exec(admin, "GRANT CONNECT ON DATABASE rdsadmin TO test_gateway")
		if err := verifyRDSPlatformBoundary(ctx, managed, request.Candidates); err == nil {
			t.Fatal("platform CONNECT exception accepted")
		}
		exec(admin, "REVOKE CONNECT ON DATABASE rdsadmin FROM test_gateway")
		exec(admin, "CREATE TABLE public.managed_shared_fact(id integer); GRANT SELECT ON public.managed_shared_fact TO test_gateway")
		if err := rejectCrossDatabaseRoles(ctx, managed, request.Candidates, true); err == nil || err.Error() != "TOKEN_RELEASE_CROSS_DATABASE_ROLE_SHARED" {
			t.Fatal("cross-database read-only ACL dependency accepted")
		}
		exec(admin, "DROP TABLE public.managed_shared_fact")
	})
	t.Run("managed-fence-and-prepare-use-real-non-superuser-authority", func(t *testing.T) {
		fence := releaseRequest{Operation: "fence", Database: database, Approval: request.Approval, Maintenance: request.Maintenance,
			Retired: []string{"retired_fixture_writer"}, Provider: "aliyun-rds", ExpectedDBA: managedConfig.User, ExpectedHost: managedConfig.Host, Migrations: request.Migrations}
		if _, err := executeRelease(ctx, managed, fence); err != nil {
			t.Fatal(err)
		}
		var open bool
		if admin.QueryRow(ctx, "SELECT rolcanlogin FROM pg_roles WHERE rolname='retired_fixture_writer'").Scan(&open) != nil || !open {
			t.Fatal("default fence changed a login")
		}
		fence.Apply = true
		if result, err := executeRelease(ctx, managed, fence); err != nil || !result.Applied {
			t.Fatalf("managed fence failed: %v", err)
		}
		prepare := request
		prepare.Provider, prepare.ExpectedDBA, prepare.ExpectedHost = "aliyun-rds", managedConfig.User, managedConfig.Host
		if result, err := executeRelease(ctx, managed, prepare); err != nil || !result.Applied {
			t.Fatalf("managed existing-storage preparation failed: %v", err)
		}
	})
	// Seed the published release through the actual activation/audit triggers;
	// later checks execute the real new governance CLI through its wrapper.
	exec(managed, `SET ROLE token_schema_owner; UPDATE public.token_command_governance_activation SET state='ACTIVE',epoch=1,policy_set_digest='f2b71d1b118a69974dad6154dcd50dd1ec06f36c20f8c1cf4316ecd799ab74ee',handler_set_digest='ee635163dca48eefc89961762ee6815eb745c9225b2cef54406b0a9cebb97a08',approved_runtime_roles=ARRAY['test_credential','test_gateway'],operator_id='fixture',reason='actual enable boundary fixture',request_id='11111111-1111-4111-8111-111111111111',updated_at=clock_timestamp() WHERE singleton; RESET ROLE`)
	credentials := managedCredentials{Version: 1, Host: managedConfig.Host, Database: database, ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Second), Credentials: []managedCredential{{Role: "test_credential", Password: strings.Repeat("fixture-only-a", 3)}, {Role: "test_gateway", Password: strings.Repeat("fixture-only-b", 3)}}}
	file := filepath.Join(t.TempDir(), "credentials.json")
	writeCredentials := func() {
		body, _ := json.Marshal(credentials)
		if os.WriteFile(file, body, 0600) != nil {
			t.Fatal("private fixture credentials unavailable")
		}
	}
	writeCredentials()
	enable := releaseRequest{Operation: "enable", Database: database, Approval: request.Approval, Maintenance: request.Maintenance, ExpectedEpoch: 1, Candidates: request.Candidates, Provider: "aliyun-rds", ExpectedDBA: managedConfig.User, ExpectedHost: managedConfig.Host, CredentialsFile: file, Migrations: request.Migrations}
	t.Run("hardened-business-dml-does-not-authorize-control-or-audit-rights", func(t *testing.T) {
		for _, mutation := range []struct{ right, table string }{{"UPDATE", "token_audit_logs"}, {"UPDATE", "schema_migrations"}, {"TRUNCATE", "usage_requests"}} {
			exec(managed, "SET ROLE token_schema_owner; GRANT "+mutation.right+" ON public."+pgx.Identifier{mutation.table}.Sanitize()+" TO test_credential; RESET ROLE")
			_, err := executeRelease(ctx, managed, enable)
			exec(managed, "SET ROLE token_schema_owner; REVOKE "+mutation.right+" ON public."+pgx.Identifier{mutation.table}.Sanitize()+" FROM test_credential; RESET ROLE")
			if err == nil || err.Error() != "TOKEN_RELEASE_CANDIDATE_ROLE_UNSAFE" {
				t.Fatalf("forbidden %s on %s accepted: %v", mutation.right, mutation.table, err)
			}
		}
		exec(managed, "SET ROLE token_schema_owner; GRANT CREATE ON SCHEMA public TO test_credential; SET ROLE test_credential; CREATE TABLE public.candidate_owned_fixture(id integer); SET ROLE token_schema_owner; REVOKE CREATE ON SCHEMA public FROM test_credential; RESET ROLE")
		_, err := executeRelease(ctx, managed, enable)
		exec(managed, "DROP TABLE public.candidate_owned_fixture")
		if err == nil || err.Error() != "TOKEN_RELEASE_CANDIDATE_ROLE_UNSAFE" {
			t.Fatal("runtime-owned object accepted")
		}
	})
	t.Run("dry-run-does-not-install-credentials-or-enable", func(t *testing.T) {
		originalExpiry := credentials.ExpiresAt
		credentials.ExpiresAt = time.Now().UTC().Add(30 * time.Second).Truncate(time.Second)
		writeCredentials()
		if _, err := executeRelease(ctx, managed, enable); err == nil || err.Error() != "TOKEN_RELEASE_CREDENTIAL_BINDING_INVALID" {
			t.Fatal("new credential admission accepted less than one minute")
		}
		credentials.ExpiresAt = originalExpiry
		writeCredentials()
		if _, err := executeRelease(ctx, managed, enable); err != nil {
			t.Fatal(err)
		}
		var untouched bool
		if err := admin.QueryRow(ctx, `SELECT bool_and(NOT rolcanlogin AND rolpassword IS NULL) FROM pg_authid WHERE rolname=ANY($1)`, request.Candidates).Scan(&untouched); err != nil || !untouched {
			t.Fatal("dry-run changed credentials or logins")
		}
	})
	t.Run("scram-commit-real-tls-probe-and-read-only-retry", func(t *testing.T) {
		enable.Apply = true
		result, err := executeRelease(ctx, managed, enable)
		if err != nil || !result.Applied {
			t.Fatalf("real managed enable failed: %v", err)
		}
		var before string
		if err := admin.QueryRow(ctx, `SELECT string_agg(rolpassword,',' ORDER BY rolname) FROM pg_authid WHERE rolname=ANY($1)`, request.Candidates).Scan(&before); err != nil {
			t.Fatal(err)
		}
		result, err = executeRelease(ctx, managed, enable)
		if err != nil || result.Applied || result.Decision != "ALREADY_ENABLED" {
			t.Fatal("same-epoch authenticated retry was not read-only")
		}
		selected, err := readManagedCredentials(enable)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(file, file+".after-commit"); err != nil {
			t.Fatal(err)
		}
		err = probeManagedCredentials(ctx, managed, selected)
		if restore := os.Rename(file+".after-commit", file); restore != nil {
			t.Fatal(restore)
		}
		if err != nil {
			t.Fatal("post-commit probe reopened its admission file")
		}
		credentials.Credentials[0].Password = strings.Repeat("wrong-fixture-password", 2)
		writeCredentials()
		if _, err := executeRelease(ctx, managed, enable); err == nil || err.Error() != "TOKEN_RELEASE_ENABLE_COMMITTED_AUTHENTICATION_UNCONFIRMED" {
			t.Fatal("wrong actual credential accepted")
		}
		var after string
		admin.QueryRow(ctx, `SELECT string_agg(rolpassword,',' ORDER BY rolname) FROM pg_authid WHERE rolname=ANY($1)`, request.Candidates).Scan(&after)
		if before != after {
			t.Fatal("retry rewrote credentials")
		}
		if postgrescontrol.ProbeSCRAM(ctx, config, config.User, "unused-fixture") == nil {
			t.Fatal("trust authentication passed the SCRAM probe")
		}
	})
	t.Run("post-commit-probe-failure-recovers-without-reissuing-credentials", func(t *testing.T) {
		t.Cleanup(func() {
			exec(admin, "GRANT CONNECT ON DATABASE "+pgx.Identifier{database}.Sanitize()+" TO PUBLIC")
			exec(admin, "REVOKE CONNECT ON DATABASE "+pgx.Identifier{database}.Sanitize()+" FROM test_credential,test_gateway")
		})
		exec(admin, "ALTER ROLE test_credential NOLOGIN; ALTER ROLE test_gateway NOLOGIN")
		exec(admin, "REVOKE CONNECT ON DATABASE "+pgx.Identifier{database}.Sanitize()+" FROM PUBLIC")
		result, err := executeRelease(ctx, managed, enable)
		if err == nil || err.Error() != "TOKEN_RELEASE_ENABLE_COMMITTED_AUTHENTICATION_UNCONFIRMED" || !result.Applied {
			t.Fatal("post-commit authentication failure lost its committed-state distinction")
		}
		var before string
		if admin.QueryRow(ctx, `SELECT string_agg(rolpassword,',' ORDER BY rolname) FROM pg_authid WHERE rolcanlogin AND rolname=ANY($1)`, request.Candidates).Scan(&before) != nil || before == "" {
			t.Fatal("committed role state unavailable after probe failure")
		}
		exec(admin, "GRANT CONNECT ON DATABASE "+pgx.Identifier{database}.Sanitize()+" TO test_credential,test_gateway")
		result, err = executeRelease(ctx, managed, enable)
		if err != nil || result.Applied || result.Decision != "ALREADY_ENABLED" {
			t.Fatalf("authentication recovery did not confirm the same committed state: %v", err)
		}
		var after string
		if admin.QueryRow(ctx, `SELECT string_agg(rolpassword,',' ORDER BY rolname) FROM pg_authid WHERE rolname=ANY($1)`, request.Candidates).Scan(&after) != nil || before != after {
			t.Fatal("post-commit recovery rewrote installed credentials")
		}
	})
	managed.Close(context.Background())
	t.Run("full-migration-policy-expansion-and-runtime-consumer", func(t *testing.T) {
		testGovernanceExpansionPostgres(t, ctx, admin, tokenConfig, &created)
	})
}
