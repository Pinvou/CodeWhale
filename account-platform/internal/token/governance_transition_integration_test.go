package token

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestTokenGovernanceTransitionPostgresIntegration(t *testing.T) {
	base := os.Getenv("PINVOU_TEST_TOKEN_DATABASE_URL")
	accountBase := os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL")
	if base == "" || accountBase == "" {
		t.Skip("real Token and Account PostgreSQL URLs are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	control, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close(context.Background()) })
	accountConnection, accountURL := newThreeServiceDatabase(t, ctx, accountBase, "activation_account_")
	accountMigrations, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if err = migrations.Up(ctx, accountConnection, accountMigrations); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, accountConnection)
	accountPool, err := pgxpool.New(ctx, accountURL)
	if err != nil {
		t.Fatal(err)
	}
	defer accountPool.Close()
	connection, tokenURL := newThreeServiceDatabase(t, ctx, base, "token_saga_")
	loaded, err := migrations.Load("../../migrations/token")
	if err != nil {
		t.Fatal(err)
	}
	activationIndex := -1
	for index := range loaded {
		if loaded[index].Version == 31 {
			activationIndex = index
			break
		}
	}
	if activationIndex < 0 || loaded[len(loaded)-1].Version != 33 {
		t.Fatal("activation migration 31 and Token head 33 must both be present")
	}
	if err = migrations.Up(ctx, connection, loaded[:activationIndex]); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)
	var previousPassword *string
	var previousLogin, previousInherit bool
	if err = control.QueryRow(ctx, `SELECT rolpassword,rolcanlogin,rolinherit FROM pg_authid WHERE rolname='token_migrator'`).Scan(&previousPassword, &previousLogin, &previousInherit); err != nil {
		t.Fatal("fixture cannot preserve migrator authentication")
	}
	rolePassword := strings.ReplaceAll(uuid.NewString(), "-", "") + strings.ReplaceAll(uuid.NewString(), "-", "")
	var addedMemberships []string
	for _, role := range []string{"token_gateway", "token_cap_policy_deployer", "token_schema_owner", "pg_read_all_stats"} {
		var exists bool
		if err = control.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_auth_members m JOIN pg_roles granted ON granted.oid=m.roleid JOIN pg_roles member ON member.oid=m.member WHERE granted.rolname=$1 AND member.rolname='token_migrator')`, role).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			addedMemberships = append(addedMemberships, role)
		}
	}
	t.Cleanup(func() {
		for _, role := range addedMemberships {
			if _, err := control.Exec(context.Background(), "REVOKE "+pgx.Identifier{role}.Sanitize()+" FROM token_migrator"); err != nil {
				t.Errorf("restore fixture migrator membership: %v", err)
			}
		}
	})
	t.Cleanup(func() {
		password := "NULL"
		if previousPassword != nil {
			password = "'" + strings.ReplaceAll(*previousPassword, "'", "''") + "'"
		}
		login, inherit := "NOLOGIN", "NOINHERIT"
		if previousLogin {
			login = "LOGIN"
		}
		if previousInherit {
			inherit = "INHERIT"
		}
		if _, err := control.Exec(context.Background(), "ALTER ROLE token_migrator "+login+" "+inherit+" PASSWORD "+password); err != nil {
			t.Error("restore fixture migrator authentication failed")
		}
	})
	if _, err = connection.Exec(ctx, `RESET ROLE; GRANT token_gateway,token_cap_policy_deployer,token_schema_owner,pg_read_all_stats TO token_migrator; ALTER ROLE token_migrator LOGIN NOINHERIT PASSWORD '`+rolePassword+`'; CREATE TABLE public.activation_effect_probe(id INTEGER PRIMARY KEY); ALTER TABLE public.activation_effect_probe OWNER TO token_gateway`); err != nil {
		t.Fatal(err)
	}
	tokenConfig, err := pgx.ParseConfig(tokenURL)
	if err != nil {
		t.Fatal(err)
	}
	migratorConfig := tokenConfig.Copy()
	migratorConfig.User = "token_migrator"
	migratorConfig.Password = rolePassword
	migrator, err := pgx.ConnectConfig(ctx, migratorConfig)
	if err != nil {
		t.Fatal(err)
	}
	assertGovernanceDBIdentity(t, ctx, migrator, tokenConfig.Database, "token_migrator")
	var super bool
	if err = migrator.QueryRow(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname=session_user`).Scan(&super); err != nil || super {
		t.Fatalf("migration must be non-superuser: %v super=%v", err, super)
	}
	if _, err = migrator.Exec(ctx, `SET ROLE token_gateway`); err != nil {
		t.Fatal(err)
	}
	if err = migrations.Up(ctx, migrator, loaded[:activationIndex+1]); err != nil {
		t.Fatalf("non-super migration31: %v", err)
	}
	migrations.Unlock(ctx, migrator)
	_ = migrator.Close(ctx)
	poolConfig, err := pgxpool.ParseConfig(tokenURL)
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.ConnConfig = migratorConfig
	poolConfig.MaxConns = 1
	migratorPool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer migratorPool.Close()
	roles := []string{"activation_a_" + strings.ReplaceAll(uuid.NewString(), "-", ""), "activation_b_" + strings.ReplaceAll(uuid.NewString(), "-", ""), "activation_old_" + strings.ReplaceAll(uuid.NewString(), "-", "")}
	for _, role := range roles {
		if _, err = control.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" NOLOGIN INHERIT PASSWORD '"+rolePassword+"'; GRANT token_gateway TO "+pgx.Identifier{role}.Sanitize()); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, role := range roles {
			if _, err := control.Exec(context.Background(), "DROP ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
				t.Errorf("drop isolated activation role: %v", err)
			}
		}
	})
	// Preserve pre-existing cluster roles. Only this serial fixture temporarily
	// fences their Token write access; no production server is involved.
	rows, err := connection.Query(ctx, `SELECT r.rolname FROM pg_roles r WHERE r.rolcanlogin AND NOT r.rolsuper AND r.rolname<>'token_migrator' AND (pg_has_role(r.oid,'token_gateway','MEMBER') OR pg_has_role(r.oid,'token_cap_policy_deployer','MEMBER') OR pg_has_role(r.oid,'token_schema_owner','MEMBER') OR EXISTS(SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public' AND p.prosecdef AND has_function_privilege(r.oid,p.oid,'EXECUTE')))`)
	if err != nil {
		t.Fatal(err)
	}
	var originalLogin []string
	for rows.Next() {
		var role string
		if err = rows.Scan(&role); err != nil {
			t.Fatal(err)
		}
		originalLogin = append(originalLogin, role)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	for _, role := range originalLogin {
		if _, err = control.Exec(ctx, "ALTER ROLE "+pgx.Identifier{role}.Sanitize()+" NOLOGIN"); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, role := range originalLogin {
			_, _ = control.Exec(context.Background(), "ALTER ROLE "+pgx.Identifier{role}.Sanitize()+" LOGIN")
		}
	})
	_ = connection.Close(ctx) // No bootstrap/superuser session may bypass the drain.
	request := GovernanceTransitionRequest{ExpectedState: "PRE_ACTIVATION", ExpectedEpoch: 0, TargetState: "ACTIVE", OperatorID: "activation-integration", Reason: "Verify the reviewed candidate installation", RequestID: uuid.New(), ApprovedRuntimeRoles: roles[:1], Apply: true}
	if _, err = control.Exec(ctx, "ALTER ROLE "+pgx.Identifier{roles[2]}.Sanitize()+" LOGIN"); err != nil {
		t.Fatal(err)
	}
	if _, err = CheckOrTransitionTokenGovernance(ctx, migratorPool, accountPool, request); err == nil {
		t.Fatal("old writer LOGIN did not block activation")
	}
	oldConfig := tokenConfig.Copy()
	oldConfig.User, oldConfig.Password = roles[2], rolePassword
	oldSession, err := pgx.ConnectConfig(ctx, oldConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer oldSession.Close(context.Background())
	assertGovernanceDBIdentity(t, ctx, oldSession, tokenConfig.Database, roles[2])
	if _, err = control.Exec(ctx, "ALTER ROLE "+pgx.Identifier{roles[2]}.Sanitize()+" NOLOGIN"); err != nil {
		t.Fatal(err)
	}
	if _, err = CheckOrTransitionTokenGovernance(ctx, migratorPool, accountPool, request); err == nil {
		t.Fatal("NOLOGIN writer with an existing session did not block activation")
	}
	if err = oldSession.Close(ctx); err != nil {
		t.Fatal(err)
	}
	active, err := CheckOrTransitionTokenGovernance(ctx, migratorPool, accountPool, request)
	if err != nil || active.State != "ACTIVE" || active.Epoch != 1 {
		t.Fatalf("non-super ACTIVE: %+v %v", active, err)
	}
	if _, err = CheckOrTransitionTokenGovernance(ctx, migratorPool, accountPool, request); err == nil {
		t.Fatal("duplicate CAS advanced the epoch")
	}
	looked, err := CheckOrTransitionTokenGovernance(ctx, migratorPool, nil, GovernanceTransitionRequest{RequestID: request.RequestID})
	if err != nil || looked.Epoch != 1 {
		t.Fatalf("original transition lookup: %+v %v", looked, err)
	}
	if _, err = control.Exec(ctx, "ALTER ROLE "+pgx.Identifier{roles[0]}.Sanitize()+" LOGIN"); err != nil {
		t.Fatal(err)
	}
	runtimeConfig := poolConfig.Copy()
	runtimeConfig.ConnConfig = tokenConfig.Copy()
	runtimeConfig.ConnConfig.User = roles[0]
	runtimeConfig.ConnConfig.Password = rolePassword
	runtimeConfig.MaxConns = 2
	runtime, err := pgxpool.NewWithConfig(ctx, runtimeConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	assertGovernanceDBIdentity(t, ctx, runtime, tokenConfig.Database, roles[0])
	handlerDigest, _ := TokenCommandHandlerDigest()
	policyDigest, _ := approvalpolicy.PolicySetDigest()
	store, err := NewTokenGovernanceStore(runtime, handlerDigest, policyDigest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = runtime.Exec(ctx, `UPDATE public.token_command_governance_activation SET epoch=epoch+1`); err == nil {
		t.Fatal("runtime can write activation")
	}
	effect, err := runtime.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	selected := TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: 1}
	if err = store.LockAndCheckTokenRouteActivation(ctx, effect, selected); err != nil {
		t.Fatal(err)
	}
	if _, err = effect.Exec(ctx, `INSERT INTO activation_effect_probe(id) VALUES(1)`); err != nil {
		t.Fatal(err)
	}
	safeRequest := GovernanceTransitionRequest{ExpectedState: "ACTIVE", ExpectedEpoch: 1, TargetState: "SAFE_ROLLBACK", OperatorID: request.OperatorID, Reason: "Close writes while preserving original leases", RequestID: uuid.New(), Apply: true}
	safeDone := make(chan error, 1)
	go func() {
		_, err := CheckOrTransitionTokenGovernance(ctx, migratorPool, nil, safeRequest)
		safeDone <- err
	}()
	select {
	case err := <-safeDone:
		t.Fatalf("SAFE bypassed effect lock: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	if err = effect.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-safeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("SAFE CAS blocked after effect commit")
	}
	stale, err := runtime.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.LockAndCheckTokenRouteActivation(ctx, stale, selected); err == nil {
		t.Fatal("old epoch survived committed SAFE")
	}
	_ = stale.Rollback(ctx)
	var effects int
	if err = runtime.QueryRow(ctx, `SELECT count(*) FROM activation_effect_probe`).Scan(&effects); err != nil || effects != 1 {
		t.Fatalf("linearized effects=%d err=%v", effects, err)
	}
	runtime.Close()
	if _, err = control.Exec(ctx, "ALTER ROLE "+pgx.Identifier{roles[0]}.Sanitize()+" NOLOGIN"); err != nil {
		t.Fatal(err)
	}
	resume := request
	resume.ExpectedState = "SAFE_ROLLBACK"
	resume.ExpectedEpoch = 2
	resume.RequestID = uuid.New()
	if _, err = CheckOrTransitionTokenGovernance(ctx, migratorPool, accountPool, resume); err == nil {
		t.Fatal("retired credential reused by resumed ACTIVE")
	}
	resume.ApprovedRuntimeRoles = roles[1:2]
	if got, err := CheckOrTransitionTokenGovernance(ctx, migratorPool, accountPool, resume); err != nil || got.Epoch != 3 || got.State != "ACTIVE" {
		t.Fatalf("new release resume: %+v %v", got, err)
	}
}

func assertGovernanceDBIdentity(t *testing.T, ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, database, user string) {
	t.Helper()
	var actualDatabase, session, current string
	if err := q.QueryRow(ctx, `SELECT current_database(),session_user,current_user`).Scan(&actualDatabase, &session, &current); err != nil || actualDatabase != database || session != user || current != user {
		t.Fatalf("actual identity database=%s session=%s current=%s error=%v", actualDatabase, session, current, err)
	}
}
