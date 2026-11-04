package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/cifixture"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/token"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Uses the real Account and Token migration packages and non-superuser control
// identities. The original release's ACTIVE/SAFE rows are controlled fixture
// inputs through the actual immutable triggers, not an old-CLI execution claim.
func testGovernanceExpansionPostgres(t *testing.T, ctx context.Context, admin *pgx.Conn, tokenConfig *pgx.ConnConfig, created *[]string) {
	t.Helper()
	exec := func(connection *pgx.Conn, sql string, args ...any) {
		t.Helper()
		if _, err := connection.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	accountRoles := []string{"account_service", "account_migrator", "account_approval_control_owner", "account_approval_authorization_owner", "account_approval_writer", "account_approval_backfill", "account_approval_relay", "account_approval_recovery", "account_policy_fixture"}
	var count int
	if admin.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname=ANY($1)`, accountRoles).Scan(&count) != nil || count != 0 {
		t.Fatal("Account fixture roles must be absent")
	}
	for _, role := range accountRoles {
		options := " LOGIN NOINHERIT"
		if strings.HasSuffix(role, "_owner") || role == "account_approval_recovery" {
			options = " NOLOGIN NOINHERIT"
		}
		if role == "account_service" {
			options = " LOGIN INHERIT"
		}
		exec(admin, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+options)
		*created = append(*created, role)
	}
	exec(admin, `GRANT account_service,account_approval_control_owner TO account_migrator; GRANT account_approval_authorization_owner TO account_migrator WITH INHERIT FALSE,SET TRUE,ADMIN TRUE`)
	accountDatabase := "managed_account_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	var fixtureDatabase *cifixture.Database
	fixtureDatabaseCreated, creationError := cifixture.CreateDatabase(ctx, admin, accountDatabase, "CREATE DATABASE "+pgx.Identifier{accountDatabase}.Sanitize()+" OWNER account_service")
	fixtureDatabase = fixtureDatabaseCreated
	if err := creationError; err != nil {
		t.Fatal(err)
	}
	defer func() {
		clean, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := cifixture.DropDatabase(clean, admin, fixtureDatabase, "account_service"); err != nil {
			t.Error("Account fixture cleanup failed")
		}
	}()
	exec(admin, "GRANT CREATE ON DATABASE "+pgx.Identifier{accountDatabase}.Sanitize()+" TO account_migrator")
	accountConfig := tokenConfig.Copy()
	accountConfig.Database, accountConfig.User = accountDatabase, "account_migrator"
	accountMigration, err := pgx.ConnectConfig(ctx, accountConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer accountMigration.Close(context.Background())
	accountLoaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	exec(accountMigration, "SET ROLE account_service")
	err = accountschema.Up(ctx, accountMigration, accountLoaded)
	migrations.Unlock(ctx, accountMigration)
	accountMigration.Close(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	accountOwnerConfig := accountConfig.Copy()
	accountOwnerConfig.User = admin.Config().User
	accountOwner, err := pgx.ConnectConfig(ctx, accountOwnerConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer accountOwner.Close(context.Background())
	exec(accountOwner, "GRANT USAGE ON SCHEMA public TO account_policy_fixture; GRANT SELECT,UPDATE ON public.approval_command_policies TO account_policy_fixture")
	accountConfig.User = "account_policy_fixture"
	openPool := func(config *pgx.ConnConfig) *pgxpool.Pool {
		t.Helper()
		poolConfig, err := pgxpool.ParseConfig(config.ConnString())
		if err != nil {
			t.Fatal(err)
		}
		poolConfig.ConnConfig = config.Copy()
		poolConfig.MaxConns = 1
		pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
		if err != nil {
			t.Fatal(err)
		}
		return pool
	}
	accountPool := openPool(accountConfig)
	defer accountPool.Close()
	rootToken, err := pgx.ConnectConfig(ctx, tokenConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer rootToken.Close(context.Background())
	exec(admin, "ALTER ROLE test_credential NOLOGIN; ALTER ROLE test_gateway NOLOGIN; ALTER ROLE token_migrator LOGIN")
	newRoles := []string{"upgrade_credential", "upgrade_gateway"}
	for _, role := range newRoles {
		exec(admin, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" NOLOGIN INHERIT")
		*created = append(*created, role)
		exec(admin, "GRANT token_gateway TO "+pgx.Identifier{role}.Sanitize())
	}
	// Transition the old published evidence to SAFE without altering either
	// digest, its approved role set, or any previously recorded transition.
	exec(rootToken, `SET ROLE token_schema_owner; INSERT INTO public.outbox_events(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at) VALUES('22222222-2222-4222-8222-222222222222','token.usage-reconciliation.retry.v1','22222222-2222-4222-8222-222222222222',1,'{"fixture_history":true}','upgrade-fixture',clock_timestamp()); UPDATE public.token_command_governance_activation SET state='SAFE_ROLLBACK',epoch=2,operator_id='fixture',reason='old published release enters SAFE',request_id='33333333-3333-4333-8333-333333333333',updated_at=clock_timestamp() WHERE singleton; RESET ROLE`)
	rootToken.Close(context.Background())
	migratorConfig := tokenConfig.Copy()
	migratorConfig.User = "token_migrator"
	request := token.GovernanceTransitionRequest{ExpectedState: "SAFE_ROLLBACK", ExpectedEpoch: 2, TargetState: "ACTIVE", OperatorID: "fixture", Reason: "explicit verified lifecycle policy upgrade", RequestID: uuid.New(), ApprovedRuntimeRoles: newRoles, PolicyUpgrade: approvalpolicy.LifecyclePolicySetUpgradeV1}
	runGovernanceExpansionWrapper(t, ctx, migratorConfig, accountConfig, request, false)
	tokenPool := openPool(migratorConfig)
	defer tokenPool.Close()
	t.Run("unknown-contract-and-stale-epoch-fail-without-write", func(t *testing.T) {
		for _, mutate := range []func(*token.GovernanceTransitionRequest){func(r *token.GovernanceTransitionRequest) { r.PolicyUpgrade = "" }, func(r *token.GovernanceTransitionRequest) { r.PolicyUpgrade = "unknown" }, func(r *token.GovernanceTransitionRequest) { r.ExpectedEpoch = 1 }} {
			input := request
			mutate(&input)
			input.Apply = true
			if _, err := token.CheckOrTransitionTokenGovernance(ctx, tokenPool, accountPool, input); err == nil {
				t.Fatal("unapproved contract or stale epoch accepted")
			}
		}
	})
	t.Run("dry-run-preserves-old-state", func(t *testing.T) {
		value, err := token.CheckOrTransitionTokenGovernance(ctx, tokenPool, accountPool, request)
		if err != nil || value.State != "SAFE_ROLLBACK" || value.Epoch != 2 {
			t.Fatalf("expansion check failed: %v", err)
		}
	})
	t.Run("account-lock-held-during-token-lock-wait-and-other-dba-refused", func(t *testing.T) {
		blocker, err := pgx.ConnectConfig(ctx, tokenConfig)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Close(context.Background())
		tx, err := blocker.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		if _, err := tx.Exec(ctx, "SELECT singleton FROM public.token_command_governance_activation FOR UPDATE"); err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		request.Apply = true
		go func() {
			_, err := token.CheckOrTransitionTokenGovernance(ctx, tokenPool, accountPool, request)
			result <- err
		}()
		deadline := time.Now().Add(5 * time.Second)
		for {
			var waiting bool
			if admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=$1 AND usename='token_migrator' AND wait_event_type='Lock')`, tokenConfig.Database).Scan(&waiting) != nil {
				t.Fatal("actual lock wait unavailable")
			}
			if waiting {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("governance did not reach the database lock barrier")
			}
		}
		policyTx, err := accountOwner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		policyTx.Exec(ctx, "SET LOCAL lock_timeout='100ms'")
		_, err = policyTx.Exec(ctx, "UPDATE public.approval_command_policies SET policy_version=policy_version WHERE command_type='WALLET_ADJUSTMENT'")
		policyTx.Rollback(ctx)
		var pgError *pgconn.PgError
		if !errors.As(err, &pgError) || pgError.Code != "55P03" {
			t.Fatal("authoritative policy SHARE lock was released during Token wait")
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		// Deliberately retain this other DBA connection. The strict writer
		// guard must reject the attempt, keeping SAFE and its old audit intact.
		if err := <-result; err == nil {
			t.Fatal("another Token DBA session was silently ignored")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		var otherSessions int
		if admin.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=$1 AND usename<>'token_migrator'`, tokenConfig.Database).Scan(&otherSessions) != nil {
			t.Fatal("fixture session cleanup cannot be verified")
		}
		if otherSessions == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("other fixture DBA session remains")
		}
	}
	t.Run("same-request-recovery-cas-audit-and-original-result", func(t *testing.T) {
		tokenPool.Close()
		runGovernanceExpansionWrapper(t, ctx, migratorConfig, accountConfig, request, true)
		tokenPool = openPool(migratorConfig)
		defer tokenPool.Close()
		value, err := token.CheckOrTransitionTokenGovernance(ctx, tokenPool, nil, token.GovernanceTransitionRequest{RequestID: request.RequestID})
		if err != nil || value.State != "ACTIVE" || value.Epoch != 3 {
			t.Fatalf("controlled expansion failed: %v", err)
		}
		if _, err := token.CheckOrTransitionTokenGovernance(ctx, tokenPool, accountPool, request); err == nil {
			t.Fatal("stale repeated write accepted")
		}
		lookup, err := token.CheckOrTransitionTokenGovernance(ctx, tokenPool, nil, token.GovernanceTransitionRequest{RequestID: request.RequestID})
		if err != nil || lookup.Epoch != 3 || lookup.PolicySetDigest != value.PolicySetDigest {
			t.Fatal("original committed result unavailable")
		}
		tx, err := tokenPool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE token_schema_owner"); err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		var preserved bool
		if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM public.token_command_governance_transitions)=3 AND (SELECT count(*) FROM public.token_command_governance_transitions WHERE epoch IN(1,2) AND policy_set_digest='f2b71d1b118a69974dad6154dcd50dd1ec06f36c20f8c1cf4316ecd799ab74ee')=2 AND (SELECT count(*) FROM public.outbox_events WHERE event_id='22222222-2222-4222-8222-222222222222')=1`).Scan(&preserved); err != nil {
			t.Fatalf("trusted audit verification query failed: %v", err)
		}
		if !preserved {
			t.Fatal("original governance audit or event was rewritten")
		}
		_, err = tx.Exec(ctx, "DELETE FROM public.token_command_governance_transitions WHERE epoch=1")
		tx.Rollback(ctx)
		var pgError *pgconn.PgError
		if !errors.As(err, &pgError) || pgError.Code != "55000" {
			t.Fatal("historical transition became mutable")
		}
	})
	// Complete the same upgraded generation: managed enable must accept the
	// newly approved roles at ACTIVE/3, install both credentials, authenticate
	// them, and make the same epoch consumable by a real runtime identity.
	tokenPool.Close()
	exec(admin, "ALTER ROLE token_migrator NOLOGIN")
	for _, role := range newRoles {
		exec(admin, "GRANT "+pgx.Identifier{role}.Sanitize()+" TO managed_dba_fixture WITH ADMIN OPTION")
	}
	credentials := managedCredentials{Version: 1, Host: tokenConfig.Host, Database: tokenConfig.Database,
		ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Second), Credentials: []managedCredential{
			{Role: newRoles[0], Password: strings.Repeat("runtime-fixture-", 3)},
			{Role: newRoles[1], Password: strings.Repeat("gateway-fixture-", 3)},
		}}
	file := filepath.Join(t.TempDir(), "upgraded-generation.json")
	body, _ := json.Marshal(credentials)
	if err := os.WriteFile(file, body, 0600); err != nil {
		t.Fatal("upgraded-generation credential fixture unavailable")
	}
	managedConfig := tokenConfig.Copy()
	managedConfig.User = "managed_dba_fixture"
	managed, err := pgx.ConnectConfig(ctx, managedConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("upgraded-epoch-managed-enable-and-repeat", func(t *testing.T) {
		enable := releaseRequest{Operation: "enable", Database: tokenConfig.Database, Approval: "fixture-approval", Maintenance: "fixture-exclusive-window",
			ExpectedEpoch: 3, Candidates: newRoles, Apply: true, Provider: "aliyun-rds", ExpectedDBA: managedConfig.User,
			ExpectedHost: managedConfig.Host, CredentialsFile: file, Migrations: "../../migrations/token"}
		if result, err := executeRelease(ctx, managed, enable); err != nil || !result.Applied {
			t.Fatalf("upgraded epoch managed enable failed: %v", err)
		}
		if result, err := executeRelease(ctx, managed, enable); err != nil || result.Applied || result.Decision != "ALREADY_ENABLED" {
			t.Fatalf("upgraded epoch repeat did not confirm the same commit: %v", err)
		}
	})
	managed.Close(context.Background())
	runtimeConfig := tokenConfig.Copy()
	runtimeConfig.User = newRoles[0]
	runtimeConfig.Password = credentials.Credentials[0].Password
	runtimePool := openPool(runtimeConfig)
	defer runtimePool.Close()
	handler, _ := token.TokenCommandHandlerDigest()
	policy, _ := approvalpolicy.PolicySetDigest()
	store, err := token.NewTokenGovernanceStore(runtimePool, handler, policy)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(ctx)
	if err != nil || snapshot.State != "ACTIVE" || snapshot.Epoch != 3 {
		t.Fatal("real runtime identity rejected the expanded activation")
	}
	tx, err := runtimePool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err := store.LockAndCheckTokenRouteActivation(ctx, tx, token.TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: 3}); err != nil {
		t.Fatal(err)
	}
}
