package main

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/token"
	"github.com/Pinvou/pinvou3/account-platform/internal/tokenmigrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This test owns fixed control roles and MUST use a dedicated disposable cluster.
// It refuses a populated role namespace instead of altering another test's roles.
func TestTokenReleasePreparationPostgresIntegration(t *testing.T) {
	runTokenReleasePreparation(t, nil)
}

func TestTokenReleaseOperationsPostgresIntegration(t *testing.T) {
	artifacts := newReleaseCLIArtifacts(t)
	runTokenReleasePreparation(t, artifacts)
}

func runTokenReleasePreparation(t *testing.T, artifacts *releaseCLIArtifacts) {
	dsn := os.Getenv("PINVOU_TEST_RELEASE_DATABASE_URL")
	if dsn == "" {
		t.Skip("PINVOU_TEST_RELEASE_DATABASE_URL dedicated PostgreSQL cluster is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	fixed := []string{"token_gateway", "token_schema_owner", "token_cap_policy_deployer", "token_migrator", "token_policy_operator"}
	var present int
	if err = admin.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname=ANY($1::text[])`, fixed).Scan(&present); err != nil || present != 0 {
		t.Fatal("dedicated cluster must not contain existing Token control roles")
	}
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	database, accountDatabase := "release_token_"+suffix, "release_account_"+suffix
	candidates := []string{"token_credential_" + suffix, "token_gateway_" + suffix}
	oldMember, accountReader := "token_old_"+suffix, "policy_reader_"+suffix
	createdRoles := append(append([]string{}, fixed...), append(candidates, oldMember, accountReader)...)
	quote := func(name string) string { return pgx.Identifier{name}.Sanitize() }
	exec := func(connection interface {
		Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	}, sql string, args ...any) {
		t.Helper()
		if _, err := connection.Exec(ctx, sql, args...); err != nil {
			var pg *pgconn.PgError
			if errors.As(err, &pg) {
				t.Fatalf("fixture SQL failed: %s %s", pg.Code, pg.Message)
			}
			t.Fatalf("fixture SQL failed: %T", err)
		}
	}
	exec(admin, `CREATE ROLE token_gateway LOGIN`)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		for _, name := range []string{database, accountDatabase} {
			_, _ = admin.Exec(cleanup, "DROP DATABASE IF EXISTS "+quote(name)+" WITH (FORCE)")
		}
		for i := len(createdRoles) - 1; i >= 0; i-- {
			_, _ = admin.Exec(cleanup, "DROP ROLE IF EXISTS "+quote(createdRoles[i]))
		}
	})
	exec(admin, "CREATE DATABASE "+quote(database)+" OWNER token_gateway")
	exec(admin, "CREATE DATABASE "+quote(accountDatabase))
	connect := func(db, user string) *pgx.Conn {
		t.Helper()
		config := admin.Config().Copy()
		config.Database = db
		config.User = user
		config.Password = ""
		config.RuntimeParams["search_path"] = "pg_catalog,public"
		connection, err := pgx.ConnectConfig(ctx, config)
		if err != nil {
			t.Fatalf("fixture connection failed: %T", err)
		}
		return connection
	}
	control := connect(database, admin.Config().User)
	defer func() { _ = control.Close(context.Background()) }()
	account := connect(accountDatabase, admin.Config().User)
	defer account.Close(context.Background())
	loaded, err := migrations.Load("../../migrations/token")
	if err != nil {
		t.Fatal(err)
	}
	accountLoaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	exec(control, `CREATE EXTENSION pgcrypto WITH SCHEMA public`)
	exec(control, `SET search_path TO public`)
	exec(control, `SET ROLE token_gateway`)
	if err = migrations.Up(ctx, control, loaded[:22]); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, control)
	exec(control, `CREATE TABLE public.release_fixture_history(id INTEGER PRIMARY KEY,payload JSONB NOT NULL)`)
	exec(control, `INSERT INTO public.release_fixture_history VALUES(1,'{"retained":true}')`)
	exec(control, `RESET ROLE`)
	exec(control, `SET search_path TO pg_catalog,public`)
	exec(account, `SET search_path TO public`)
	accountReleaseBoundary := 0
	for index, migration := range accountLoaded {
		if migration.Version > 49 {
			break
		}
		accountReleaseBoundary = index + 1
	}
	if accountReleaseBoundary == 0 || accountLoaded[accountReleaseBoundary-1].Version != 49 {
		t.Fatal("account release fixture is missing migration 49")
	}
	// The release gate only locks approval_command_policies. Account 50+ owns a
	// separate multi-role control plane and must not be fabricated by this
	// dedicated Token release fixture.
	if err = migrations.Up(ctx, account, accountLoaded[:accountReleaseBoundary]); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, account)
	exec(account, `SET search_path TO pg_catalog,public`)
	r := releaseRequest{Operation: "inventory", Database: database, Migrations: "../../migrations/token"}
	facts := func() string {
		t.Helper()
		var value string
		if err := control.QueryRow(ctx, `SELECT jsonb_build_object(
 'roles',(SELECT jsonb_agg(x ORDER BY rolname) FROM (SELECT rolname,rolcanlogin,rolsuper,rolinherit,rolcreaterole,rolcreatedb,rolreplication,rolbypassrls,rolvaliduntil FROM pg_roles) x),
 'members',(SELECT jsonb_agg(x ORDER BY roleid,member) FROM pg_auth_members x),
 'databases',(SELECT jsonb_agg(x ORDER BY datname) FROM (SELECT datname,datdba FROM pg_database) x),
 'schema',(SELECT jsonb_agg(x) FROM (SELECT nspname,nspowner,nspacl FROM pg_namespace WHERE nspname='public') x),
 'tables',(SELECT jsonb_agg(x ORDER BY relname) FROM (SELECT c.relname,c.relowner,c.relacl FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN ('r','p')) x),
 'history',(SELECT jsonb_agg(x ORDER BY id) FROM public.release_fixture_history x))::text`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	call := func(request releaseRequest, wantError string) releaseEvidence {
		t.Helper()
		before := ""
		if wantError != "" && wantError != "FENCED_SESSIONS_REMAIN" {
			before = facts()
			request.Apply = true
		}
		result, err := executeRelease(ctx, control, request)
		if wantError == "" {
			if err != nil {
				t.Fatal(err)
			}
		} else if err == nil || err.Error() != "TOKEN_RELEASE_"+wantError {
			t.Fatalf("want %s, got %v", wantError, err)
		}
		if before != "" && facts() != before {
			t.Fatal("rejected operation changed durable roles, grants, owners or history")
		}
		return result
	}
	if result := call(r, ""); result.State != "NOT_INSTALLED" || result.MigrationVersion != 22 || result.Applied {
		t.Fatal("old installation was fabricated as PRE or mutated")
	}
	r.Operation, r.Approval, r.Maintenance, r.Candidates = "prepare", "test-approval", "test-window", candidates
	call(r, "UNAPPROVED_WRITER_PRESENT")
	exec(admin, "CREATE ROLE "+quote(oldMember)+" LOGIN NOINHERIT")
	exec(admin, "GRANT token_gateway TO "+quote(oldMember))
	fence := r
	fence.Operation, fence.Candidates, fence.Retired = "fence", nil, []string{"token_gateway"}
	call(fence, "UNAPPROVED_WRITER_PRESENT")
	fence.Retired = []string{"token_gateway", oldMember}
	slices.Sort(fence.Retired)
	call(fence, "") // No --apply: login properties remain unchanged.
	var login bool
	if err = control.QueryRow(ctx, `SELECT rolcanlogin FROM pg_roles WHERE rolname='token_gateway'`).Scan(&login); err != nil || !login {
		t.Fatal("check mode mutated role")
	}
	old := connect(database, oldMember)
	defer old.Close(context.Background())
	fence.Apply = true
	call(fence, "FENCED_SESSIONS_REMAIN")
	if err = control.QueryRow(ctx, `SELECT rolcanlogin FROM pg_roles WHERE rolname='token_gateway'`).Scan(&login); err != nil || login {
		t.Fatal("NOLOGIN not retained after incomplete cleanup")
	}
	r.Retired = fence.Retired
	call(r, "WRITERS_NOT_QUIESCED")
	fence.Terminate = true
	call(fence, "")
	_ = old.Close(ctx)
	otherDBA := connect(database, admin.Config().User)
	inventoryRequest := releaseRequest{Operation: "inventory", Database: database, Migrations: r.Migrations}
	if result := call(inventoryRequest, ""); result.WriterSessions == 0 {
		t.Fatal("same-name other DBA connection omitted from inventory")
	}
	call(r, "OTHER_DBA_SESSION_PRESENT")
	_ = otherDBA.Close(ctx)
	wrongDatabase := r
	wrongDatabase.Database = accountDatabase
	call(wrongDatabase, "DATABASE_OR_IDENTITY_MISMATCH")
	exec(account, `GRANT UPDATE ON public.approval_command_policies TO token_gateway`)
	call(r, "CROSS_DATABASE_ROLE_SHARED")
	exec(account, `REVOKE UPDATE ON public.approval_command_policies FROM token_gateway`)
	// All affected control roles and their explicit ACL/ownership dependencies
	// are cluster-global, including read-only/column/default privileges.
	exec(admin, `CREATE ROLE token_schema_owner NOLOGIN`)
	for _, pair := range [][2]string{
		{`GRANT SELECT ON public.approval_command_policies TO token_schema_owner`, `REVOKE SELECT ON public.approval_command_policies FROM token_schema_owner`},
		{`GRANT SELECT(command_type) ON public.approval_command_policies TO token_schema_owner`, `REVOKE SELECT(command_type) ON public.approval_command_policies FROM token_schema_owner`},
		{`GRANT CREATE ON SCHEMA public TO token_schema_owner`, `REVOKE CREATE ON SCHEMA public FROM token_schema_owner`},
		{`ALTER SCHEMA public OWNER TO token_schema_owner`, `ALTER SCHEMA public OWNER TO pg_database_owner`},
		{`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO token_schema_owner`, `ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE SELECT ON TABLES FROM token_schema_owner`},
	} {
		exec(account, pair[0])
		call(r, "CROSS_DATABASE_ROLE_SHARED")
		exec(account, pair[1])
	}
	exec(admin, `DROP ROLE token_schema_owner`)
	exec(admin, "ALTER DATABASE "+quote(accountDatabase)+" ALLOW_CONNECTIONS false")
	call(r, "CROSS_DATABASE_INVENTORY_UNAVAILABLE")
	exec(admin, "ALTER DATABASE "+quote(accountDatabase)+" ALLOW_CONNECTIONS true")
	exec(admin, "CREATE ROLE "+quote(candidates[0])+" LOGIN")
	call(r, "EXISTING_ROLE_CONFLICT")
	exec(admin, "DROP ROLE "+quote(candidates[0]))
	call(r, "")
	if err = control.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='token_schema_owner')`).Scan(&login); err != nil || login {
		t.Fatal("prepare check created roles")
	}
	r.Apply = true
	call(r, "")
	exec(admin, "GRANT token_schema_owner TO "+quote(candidates[0]))
	call(r, "EXISTING_ROLE_MEMBERSHIP_CONFLICT")
	exec(admin, "REVOKE token_schema_owner FROM "+quote(candidates[0]))
	exec(admin, "GRANT token_gateway TO "+quote(candidates[0])+" WITH INHERIT FALSE")
	call(r, "CANDIDATE_ROLE_CONFLICT")
	exec(admin, "GRANT token_gateway TO "+quote(candidates[0])+" WITH INHERIT TRUE")
	var owner string
	if err = control.QueryRow(ctx, `SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname=current_database()`).Scan(&owner); err != nil || owner != "token_schema_owner" {
		t.Fatal("existing volume database owner not prepared")
	}
	if err = control.QueryRow(ctx, `SELECT has_schema_privilege('token_gateway','public','CREATE')`).Scan(&login); err != nil || !login {
		t.Fatal("pre-29 migration CREATE missing")
	}
	call(r, "") // Same NOLOGIN candidate preparation is idempotent.
	// Existing authenticated deployment identities do the real forward migration.
	exec(admin, `ALTER ROLE token_migrator LOGIN`)
	exec(admin, `ALTER ROLE token_policy_operator LOGIN`)
	if artifacts != nil {
		artifacts.migrate(t, ctx, database, releaseTestDSN(admin.Config(), database, "token_migrator"))
		var applied int
		if err = control.QueryRow(ctx, `SELECT max(version) FROM public.schema_migrations`).Scan(&applied); err != nil || applied != 34 {
			t.Fatal("real migrate wrapper did not install exact migration 34")
		}
		artifacts.cap(t, ctx, control, database, releaseTestDSN(admin.Config(), database, "token_policy_operator"), releaseTestDSN(admin.Config(), database, admin.Config().User))
	} else {
		migrator := connect(database, "token_migrator")
		exec(migrator, `SET search_path TO public`)
		exec(migrator, `SET ROLE token_gateway`)
		if err = tokenmigrations.Up(ctx, migrator, loaded[:32]); err != nil {
			t.Fatal(err)
		}
		migrations.Unlock(ctx, migrator)
		exec(control, `SET ROLE token_schema_owner; CREATE TABLE public.routing_policies_overview_health_idx(failure_probe INTEGER); RESET ROLE`)
		if err = tokenmigrations.Up(ctx, migrator, loaded[:33]); err == nil || !strings.Contains(err.Error(), "apply migration 33") {
			t.Fatalf("migration 33 failure probe was accepted: %v", err)
		}
		migrations.Unlock(ctx, migrator)
		var createLeaked, indexExists, ledgerAdvanced bool
		if err = control.QueryRow(ctx, `SELECT
 has_schema_privilege('token_gateway','public','CREATE'),
 EXISTS(SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relname='routing_policies_overview_health_idx' AND c.relkind='i'),
 EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=33)`).Scan(&createLeaked, &indexExists, &ledgerAdvanced); err != nil || createLeaked || indexExists || ledgerAdvanced {
			t.Fatalf("failed migration 33 leaked privilege=%v index=%v ledger=%v err=%v", createLeaked, indexExists, ledgerAdvanced, err)
		}
		exec(control, `SET ROLE token_schema_owner; DROP TABLE public.routing_policies_overview_health_idx; RESET ROLE`)
		if err = tokenmigrations.Up(ctx, migrator, loaded); err != nil {
			t.Fatal(err)
		}
		migrations.Unlock(ctx, migrator)
		_ = migrator.Close(ctx)
	}
	exec(admin, `ALTER ROLE token_migrator NOLOGIN`)
	exec(admin, `ALTER ROLE token_policy_operator NOLOGIN`)
	call(r, "")
	if err = control.QueryRow(ctx, `SELECT has_schema_privilege('token_gateway','public','CREATE')`).Scan(&login); err != nil || login {
		t.Fatal("post-29 preparation restored runtime CREATE")
	}
	var retained bool
	if err = control.QueryRow(ctx, `SELECT payload='{"retained":true}'::jsonb FROM public.release_fixture_history WHERE id=1`).Scan(&retained); err != nil || !retained {
		t.Fatal("history changed")
	}
	// Account SHARE uses the actual non-super authorization boundary.
	exec(admin, "CREATE ROLE "+quote(accountReader)+" LOGIN")
	exec(account, "GRANT SELECT ON public.approval_command_policies TO "+quote(accountReader))
	reader := connect(accountDatabase, accountReader)
	defer reader.Close(context.Background())
	probe := releaseRequest{Operation: "account-policy-check", Database: accountDatabase}
	if _, err = executeRelease(ctx, reader, probe); err == nil || err.Error() != "TOKEN_RELEASE_ACCOUNT_POLICY_LOCK_DENIED" {
		t.Fatalf("SELECT-only lock accepted: %v", err)
	}
	exec(account, "GRANT UPDATE ON public.approval_command_policies TO "+quote(accountReader))
	if _, err = executeRelease(ctx, reader, probe); err != nil {
		t.Fatal(err)
	}
	locked, err := account.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	exec(locked, `LOCK TABLE public.approval_command_policies IN ROW EXCLUSIVE MODE`)
	if _, err = executeRelease(ctx, reader, probe); err == nil || err.Error() != "TOKEN_RELEASE_ACCOUNT_POLICY_LOCK_DENIED" {
		t.Fatalf("concurrent policy writer accepted: %v", err)
	}
	_ = locked.Rollback(ctx)
	// Real non-super governance CLI library: no direct activation UPDATE fixture.
	exec(admin, `ALTER ROLE token_migrator LOGIN`)
	exec(admin, `GRANT pg_read_all_stats TO token_migrator`)
	tokenConfig := control.Config().Copy()
	tokenConfig.User = "token_migrator"
	tokenConfig.Password = ""
	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.ConnConfig = tokenConfig
	poolConfig.MaxConns = 1
	tokenPool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer tokenPool.Close()
	accountConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	accountConfig.ConnConfig = reader.Config().Copy()
	accountConfig.MaxConns = 1
	accountPool, err := pgxpool.NewWithConfig(ctx, accountConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer accountPool.Close()
	transition := token.GovernanceTransitionRequest{ExpectedState: "PRE_ACTIVATION", ExpectedEpoch: 0, TargetState: "ACTIVE", OperatorID: "test-operator", Reason: "isolated release preparation test", RequestID: uuid.New(), ApprovedRuntimeRoles: candidates, Apply: true}
	_ = control.Close(ctx) // The real activation inventory also fences other DBA sessions.
	if _, err = token.CheckOrTransitionTokenGovernance(ctx, tokenPool, accountPool, transition); err != nil {
		t.Fatal(err)
	}
	tokenPool.Close()
	control = connect(database, admin.Config().User)
	if artifacts != nil {
		artifacts.governance(t, ctx, control, database, accountDatabase, releaseTestDSN(admin.Config(), database, "token_migrator"), releaseTestDSN(admin.Config(), accountDatabase, accountReader), transition.RequestID)
	}
	exec(admin, `ALTER ROLE token_migrator NOLOGIN`)
	call(r, "SAFE_REQUIRED")
	enable := r
	enable.Operation, enable.ExpectedEpoch, enable.Retired = "enable", 1, nil
	call(enable, "CANDIDATE_CREDENTIAL_UNCONFIRMED")
	for _, name := range candidates {
		exec(admin, "ALTER ROLE "+quote(name)+" PASSWORD 'isolated-test-only-not-a-production-secret' VALID UNTIL '2099-01-01T00:00:00Z'")
	}
	exec(admin, "GRANT token_gateway TO "+quote(candidates[0])+" WITH INHERIT FALSE")
	call(enable, "CANDIDATE_ROLE_UNSAFE")
	exec(admin, "GRANT token_gateway TO "+quote(candidates[0])+" WITH INHERIT TRUE")
	exec(admin, "ALTER ROLE "+quote(candidates[0])+" VALID UNTIL '2000-01-01T00:00:00Z'")
	call(enable, "CANDIDATE_CREDENTIAL_UNCONFIRMED")
	exec(admin, "ALTER ROLE "+quote(candidates[0])+" VALID UNTIL '2099-01-01T00:00:00Z'")
	exec(control, "GRANT UPDATE ON public.token_account_owner_cap_policy_active TO "+quote(candidates[0]))
	call(enable, "CANDIDATE_ROLE_UNSAFE")
	exec(control, "REVOKE UPDATE ON public.token_account_owner_cap_policy_active FROM "+quote(candidates[0]))
	exec(control, "GRANT DELETE ON public.token_account_provisioning_reservations TO "+quote(candidates[0]))
	call(enable, "CANDIDATE_ROLE_UNSAFE")
	exec(control, "REVOKE DELETE ON public.token_account_provisioning_reservations FROM "+quote(candidates[0]))
	var businessDML bool
	if err = control.QueryRow(ctx, `SELECT has_table_privilege($1::text,'public.token_account_provisioning_reservations','INSERT') AND has_table_privilege($1::text,'public.token_account_provisioning_reservations','UPDATE') AND has_table_privilege($1::text,'public.token_account_provisioning_recovery_actions','INSERT') AND has_table_privilege($1::text,'public.token_account_provisioning_recovery_actions','UPDATE')`, candidates[0]).Scan(&businessDML); err != nil || !businessDML {
		t.Fatal("required business DML was not preserved")
	}
	wrong := enable
	wrong.ExpectedEpoch = 2
	call(wrong, "ACTIVE_EPOCH_OR_ROLES_MISMATCH")
	exec(admin, "ALTER ROLE "+quote(candidates[0])+" LOGIN")
	call(enable, "CANDIDATE_LOGIN_STATE_MIXED")
	exec(admin, "ALTER ROLE "+quote(candidates[0])+" NOLOGIN")
	// N-1 remains releasable with the two privileged entry points installed by
	// migrations 31 and 32. Exercise a real rollback/reapply so migration 34 is
	// genuinely absent rather than merely hidden from the release query.
	if err = migrations.DownOne(ctx, control, loaded); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, control)
	var migration34Installed bool
	if err = control.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=34)`).Scan(&migration34Installed); err != nil || migration34Installed {
		t.Fatal("migration 34 remained installed during N-1 release check")
	}
	call(enable, "")
	for _, name := range candidates {
		exec(admin, "ALTER ROLE "+quote(name)+" NOLOGIN")
	}
	if err = tokenmigrations.Up(ctx, control, loaded); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, control)
	if err = control.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=34)`).Scan(&migration34Installed); err != nil || !migration34Installed {
		t.Fatal("migration 34 was not restored after N-1 release check")
	}
	// Migration 34 adds exactly one runtime SECURITY DEFINER capability. The
	// release gate accepts only its exact schema/signature/owner/config tuple;
	// lookalikes remain release-blocking even when owned by the trusted owner.
	var backfillDefinition string
	if err = control.QueryRow(ctx, `SELECT pg_catalog.pg_get_functiondef(to_regprocedure('public.backfill_usage_history_projection(integer)'))`).Scan(&backfillDefinition); err != nil {
		t.Fatal(err)
	}
	exec(control, `SET ROLE token_schema_owner; DROP FUNCTION public.backfill_usage_history_projection(INTEGER); RESET ROLE`)
	call(enable, "CANDIDATE_ROLE_UNSAFE")
	exec(control, backfillDefinition)
	exec(control, `ALTER FUNCTION public.backfill_usage_history_projection(INTEGER) OWNER TO token_schema_owner; REVOKE ALL ON FUNCTION public.backfill_usage_history_projection(INTEGER) FROM PUBLIC; GRANT EXECUTE ON FUNCTION public.backfill_usage_history_projection(INTEGER) TO token_gateway`)
	exec(control, `ALTER FUNCTION public.backfill_usage_history_projection(INTEGER) OWNER TO token_gateway`)
	call(enable, "CANDIDATE_ROLE_UNSAFE")
	exec(control, `ALTER FUNCTION public.backfill_usage_history_projection(INTEGER) OWNER TO token_schema_owner; GRANT EXECUTE ON FUNCTION public.backfill_usage_history_projection(INTEGER) TO token_gateway`)
	exec(control, `SET ROLE token_schema_owner; ALTER FUNCTION public.backfill_usage_history_projection(INTEGER) SECURITY INVOKER; RESET ROLE`)
	call(enable, "CANDIDATE_ROLE_UNSAFE")
	exec(control, `SET ROLE token_schema_owner; ALTER FUNCTION public.backfill_usage_history_projection(INTEGER) SECURITY DEFINER; RESET ROLE`)
	exec(control, `ALTER FUNCTION public.lock_token_account_owner_cap_policy() OWNER TO token_gateway`)
	call(enable, "CANDIDATE_ROLE_UNSAFE")
	exec(control, `ALTER FUNCTION public.lock_token_account_owner_cap_policy() OWNER TO token_cap_policy_deployer; GRANT EXECUTE ON FUNCTION public.lock_token_account_owner_cap_policy() TO token_gateway`)
	exec(control, `SET ROLE token_cap_policy_deployer; ALTER FUNCTION public.lock_token_account_owner_cap_policy() SET search_path=public; RESET ROLE`)
	call(enable, "CANDIDATE_ROLE_UNSAFE")
	exec(control, `SET ROLE token_cap_policy_deployer; ALTER FUNCTION public.lock_token_account_owner_cap_policy() SET search_path=pg_catalog; RESET ROLE`)
	var capPolicyDefinition string
	if err = control.QueryRow(ctx, `SELECT pg_catalog.pg_get_functiondef(to_regprocedure('public.lock_token_account_owner_cap_policy()'))`).Scan(&capPolicyDefinition); err != nil {
		t.Fatal(err)
	}
	exec(control, `DROP FUNCTION public.lock_token_account_owner_cap_policy();
CREATE FUNCTION public.lock_token_account_owner_cap_policy(INTEGER) RETURNS BIGINT LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog AS 'SELECT $1::bigint';
ALTER FUNCTION public.lock_token_account_owner_cap_policy(INTEGER) OWNER TO token_cap_policy_deployer;
REVOKE ALL ON FUNCTION public.lock_token_account_owner_cap_policy(INTEGER) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.lock_token_account_owner_cap_policy(INTEGER) TO token_gateway`)
	call(enable, "CANDIDATE_ROLE_UNSAFE")
	exec(control, `DROP FUNCTION public.lock_token_account_owner_cap_policy(INTEGER)`)
	exec(control, capPolicyDefinition)
	exec(control, `ALTER FUNCTION public.lock_token_account_owner_cap_policy() OWNER TO token_cap_policy_deployer; REVOKE ALL ON FUNCTION public.lock_token_account_owner_cap_policy() FROM PUBLIC; GRANT EXECUTE ON FUNCTION public.lock_token_account_owner_cap_policy() TO token_gateway`)
	exec(control, `SET ROLE token_schema_owner;
CREATE SCHEMA release_candidate_unsafe AUTHORIZATION token_schema_owner;
CREATE FUNCTION release_candidate_unsafe.backfill_usage_history_projection(INTEGER) RETURNS INTEGER LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog AS 'SELECT 1';
REVOKE ALL ON FUNCTION release_candidate_unsafe.backfill_usage_history_projection(INTEGER) FROM PUBLIC;
GRANT USAGE ON SCHEMA release_candidate_unsafe TO token_gateway;
GRANT EXECUTE ON FUNCTION release_candidate_unsafe.backfill_usage_history_projection(INTEGER) TO token_gateway;
RESET ROLE`)
	call(enable, "CANDIDATE_ROLE_UNSAFE")
	exec(control, `SET ROLE token_schema_owner; DROP SCHEMA release_candidate_unsafe CASCADE; RESET ROLE`)
	exec(control, `SET ROLE token_schema_owner;
CREATE FUNCTION public.release_candidate_unsafe() RETURNS INTEGER LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog AS 'SELECT 1';
REVOKE ALL ON FUNCTION public.release_candidate_unsafe() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.release_candidate_unsafe() TO token_gateway;
RESET ROLE`)
	call(enable, "CANDIDATE_ROLE_UNSAFE")
	exec(control, `SET ROLE token_schema_owner; DROP FUNCTION public.release_candidate_unsafe(); RESET ROLE`)
	exec(control, `SET ROLE token_schema_owner;
CREATE FUNCTION public.backfill_usage_history_projection(TEXT) RETURNS INTEGER LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog AS 'SELECT 1';
REVOKE ALL ON FUNCTION public.backfill_usage_history_projection(TEXT) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.backfill_usage_history_projection(TEXT) TO token_gateway;
RESET ROLE`)
	call(enable, "CANDIDATE_ROLE_UNSAFE")
	exec(control, `SET ROLE token_schema_owner; DROP FUNCTION public.backfill_usage_history_projection(TEXT); RESET ROLE`)
	exec(control, `SET ROLE token_schema_owner; ALTER FUNCTION public.backfill_usage_history_projection(INTEGER) SET search_path=public; RESET ROLE`)
	call(enable, "CANDIDATE_ROLE_UNSAFE")
	exec(control, `SET ROLE token_schema_owner; ALTER FUNCTION public.backfill_usage_history_projection(INTEGER) SET search_path=pg_catalog; RESET ROLE`)
	call(enable, "")
	if result := call(enable, ""); result.Decision != "ALREADY_ENABLED" || result.Applied {
		t.Fatal("enable replay performed another write")
	}
	if artifacts != nil {
		artifacts.invoke(t, ctx, "cap-readiness", database, nil, map[string]any{}, map[string]string{"TOKEN_POLICY_DATABASE_URL": releaseTestDSN(admin.Config(), database, candidates[0])}, true, "")
	}
	// Enable-first ordering: a subsequent real SAFE CAS closes the generation.
	transitionOnce := func(input token.GovernanceTransitionRequest) error {
		t.Helper()
		exec(admin, `ALTER ROLE token_migrator LOGIN`)
		pool, openErr := pgxpool.NewWithConfig(ctx, poolConfig.Copy())
		if openErr != nil {
			return openErr
		}
		if input.TargetState == "ACTIVE" {
			_ = control.Close(ctx)
		}
		_, callErr := token.CheckOrTransitionTokenGovernance(ctx, pool, accountPool, input)
		pool.Close()
		exec(admin, `ALTER ROLE token_migrator NOLOGIN`)
		if input.TargetState == "ACTIVE" {
			control = connect(database, admin.Config().User)
		}
		return callErr
	}
	safe := transition
	safe.ExpectedState, safe.ExpectedEpoch, safe.TargetState, safe.RequestID = "ACTIVE", 1, "SAFE_ROLLBACK", uuid.New()
	if err = transitionOnce(safe); err != nil {
		t.Fatal(err)
	}
	call(enable, "ACTIVE_EPOCH_OR_ROLES_MISMATCH")
	retireActive := fence
	retireActive.Retired = candidates
	call(retireActive, "")
	newCandidates := []string{"token_credential_next_" + suffix, "token_gateway_next_" + suffix}
	createdRoles = append(createdRoles, newCandidates...)
	nextPrepare := r
	nextPrepare.Candidates, nextPrepare.Retired = newCandidates, candidates
	call(nextPrepare, "")
	next := transition
	next.ExpectedState, next.ExpectedEpoch, next.RequestID, next.ApprovedRuntimeRoles = "SAFE_ROLLBACK", 2, uuid.New(), newCandidates
	if err = transitionOnce(next); err != nil {
		t.Fatal(err)
	}
	for _, name := range newCandidates {
		exec(admin, "ALTER ROLE "+quote(name)+" PASSWORD 'isolated-test-only-not-a-production-secret' VALID UNTIL '2099-01-01T00:00:00Z'")
	}
	// SAFE-first ordering, deterministic PostgreSQL lock queue (not sleeps).
	blocker := connect(database, admin.Config().User)
	defer blocker.Close(context.Background())
	block, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer block.Rollback(context.Background())
	exec(block, `SELECT singleton FROM public.token_command_governance_activation FOR UPDATE`)
	exec(admin, `ALTER ROLE token_migrator LOGIN`)
	safePool, err := pgxpool.NewWithConfig(ctx, poolConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer safePool.Close()
	safeDone := make(chan error, 1)
	safe.ExpectedEpoch, safe.RequestID, safe.ApprovedRuntimeRoles = 3, uuid.New(), newCandidates
	go func() { _, err := token.CheckOrTransitionTokenGovernance(ctx, safePool, nil, safe); safeDone <- err }()
	waitLock := func(user string, pid uint32) {
		t.Helper()
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			var waiting bool
			if err := admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=$1 AND usename=$2 AND wait_event_type='Lock' AND ($3::bigint=0 OR pid=$3::bigint))`, database, user, int64(pid)).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting {
				return
			}
			select {
			case <-deadline.C:
				t.Fatal("operation did not reach real activation lock wait")
			case <-tick.C:
			}
		}
	}
	waitLock("token_migrator", 0)
	nextEnable := enable
	nextEnable.ExpectedEpoch, nextEnable.Candidates = 3, newCandidates
	enableDone := make(chan error, 1)
	go func() { _, err := executeRelease(ctx, control, nextEnable); enableDone <- err }()
	waitLock(admin.Config().User, control.PgConn().PID())
	if err = block.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-safeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("SAFE did not complete")
	}
	select {
	case err = <-enableDone:
		if err == nil || err.Error() != "TOKEN_RELEASE_ACTIVE_EPOCH_OR_ROLES_MISMATCH" {
			t.Fatalf("old epoch enabled after SAFE: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("enable did not stop")
	}
	safePool.Close()
	exec(admin, `ALTER ROLE token_migrator NOLOGIN`)
	var opened int
	if err = admin.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname=ANY($1::text[]) AND rolcanlogin`, newCandidates).Scan(&opened); err != nil || opened != 0 {
		t.Fatal("SAFE-first opened candidate roles")
	}
}
