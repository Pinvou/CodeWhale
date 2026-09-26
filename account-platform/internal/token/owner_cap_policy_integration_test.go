package token

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/tokenmigrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestTokenAccountOwnerCapPolicyPostgresIntegration is the no-SKIP CI gate
// for the privileged bootstrap/CAS contract. It uses a dedicated database so
// the SECURITY DEFINER function can keep every business object public-schema
// qualified without weakening the production search_path boundary.
func TestTokenAccountOwnerCapPolicyPostgresIntegration(t *testing.T) {
	rawURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_TOKEN_DATABASE_URL"))
	if rawURL == "" {
		t.Skip("PINVOU_TEST_TOKEN_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	configuration, err := pgx.ParseConfig(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	testDatabase := "account_platform_token_owner_cap_test"
	adminConfig := configuration.Copy()
	adminConfig.Database = "postgres"
	admin, err := pgx.ConnectConfig(ctx, adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	const migratorPassword = "token-migrator-integration-only"
	const runtimePassword = "token-runtime-integration-only"
	const operatorPassword = "token-operator-integration-only"
	if _, err = admin.Exec(ctx, `
DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='token_gateway') THEN CREATE ROLE token_gateway; END IF;
 IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='token_cap_policy_deployer') THEN CREATE ROLE token_cap_policy_deployer NOLOGIN; END IF;
 IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='token_schema_owner') THEN CREATE ROLE token_schema_owner NOLOGIN; END IF;
 IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='token_migrator') THEN CREATE ROLE token_migrator LOGIN NOINHERIT; END IF;
 IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='token_policy_operator') THEN CREATE ROLE token_policy_operator LOGIN NOINHERIT; END IF;
END $$;
ALTER ROLE token_gateway LOGIN PASSWORD '`+runtimePassword+`';
ALTER ROLE token_migrator LOGIN NOINHERIT PASSWORD '`+migratorPassword+`';
ALTER ROLE token_policy_operator LOGIN NOINHERIT PASSWORD '`+operatorPassword+`';
GRANT token_gateway,token_cap_policy_deployer,token_schema_owner TO token_migrator;`); err != nil {
		t.Fatal(err)
	}
	_, _ = admin.Exec(ctx, `DROP DATABASE IF EXISTS `+pgx.Identifier{testDatabase}.Sanitize()+` WITH (FORCE)`)
	if _, err = admin.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{testDatabase}.Sanitize()+` OWNER token_schema_owner`); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), `DROP DATABASE IF EXISTS `+pgx.Identifier{testDatabase}.Sanitize()+` WITH (FORCE)`)
	extensionConfig := configuration.Copy()
	extensionConfig.Database = testDatabase
	extensionConnection, err := pgx.ConnectConfig(ctx, extensionConfig)
	if err != nil {
		t.Fatal(err)
	}
	_, err = extensionConnection.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pgcrypto`)
	_ = extensionConnection.Close(ctx)
	if err != nil {
		t.Fatal(err)
	}
	migratorConfig := configuration.Copy()
	migratorConfig.Database, migratorConfig.User, migratorConfig.Password = testDatabase, "token_migrator", migratorPassword
	connection, err := pgx.ConnectConfig(ctx, migratorConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = connection.Exec(ctx, `SET ROLE token_schema_owner; ALTER SCHEMA public OWNER TO token_schema_owner; GRANT USAGE,CREATE ON SCHEMA public TO token_gateway; SET ROLE token_gateway`); err != nil {
		t.Fatal(err)
	}
	loaded, err := migrations.Load("../../migrations/token")
	if err != nil {
		t.Fatal(err)
	}
	if err = tokenmigrations.Up(ctx, connection, loaded); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)
	defer connection.Close(context.Background())
	assertPolicyTestIdentity(t, ctx, connection, testDatabase, "token_migrator", "token_gateway")

	runtimeConfig := configuration.Copy()
	runtimeConfig.Database, runtimeConfig.User, runtimeConfig.Password = testDatabase, "token_gateway", runtimePassword
	poolConfig, err := pgxpool.ParseConfig(runtimeConfig.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.ConnConfig = runtimeConfig
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	assertPolicyTestIdentity(t, ctx, pool, testDatabase, "token_gateway", "token_gateway")
	if _, err = loadTokenAccountOwnerCapacity(ctx, pool, "USER", uuid.New()); !errors.Is(err, ErrTokenAccountPolicyUnavailable) {
		t.Fatalf("unbootstrapped create capability err=%v", err)
	}
	downSQL, err := os.ReadFile("../../migrations/token/000029_token_account_owner_cap_policy.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	emptyDown, err := connection.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = emptyDown.Exec(ctx, string(downSQL)); err != nil {
		emptyDown.Rollback(ctx)
		t.Fatalf("empty policy down failed: %v", err)
	}
	var removed *string
	if err = emptyDown.QueryRow(ctx, `SELECT to_regclass('public.token_account_owner_cap_policy_revisions')::text`).Scan(&removed); err != nil || removed != nil {
		emptyDown.Rollback(ctx)
		t.Fatalf("empty policy down retained schema=%v err=%v", removed, err)
	}
	if err = emptyDown.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	operatorConfig := configuration.Copy()
	operatorConfig.Database, operatorConfig.User, operatorConfig.Password = testDatabase, "token_policy_operator", operatorPassword
	operatorPoolConfig, err := pgxpool.ParseConfig(operatorConfig.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	operatorPoolConfig.ConnConfig = operatorConfig
	operatorPoolConfig.MaxConns = 4
	operatorPool, err := pgxpool.NewWithConfig(ctx, operatorPoolConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer operatorPool.Close()
	assertPolicyTestIdentity(t, ctx, operatorPool, testDatabase, "token_policy_operator", "token_policy_operator")
	apply := func(max int, expected *int64, key, reason string, requestID uuid.UUID) (version int64, event uuid.UUID, err error) {
		acquired, acquireErr := operatorPool.Acquire(ctx)
		if acquireErr != nil {
			return 0, uuid.Nil, acquireErr
		}
		defer acquired.Release()
		var expectedValue any
		if expected != nil {
			expectedValue = *expected
		}
		var returnedMax int
		var configDigest, requestDigest, changedBy string
		var changedAt time.Time
		err = acquired.QueryRow(ctx, `SELECT policy_version,max_per_owner,config_digest,request_digest,outbox_event_id,changed_by,changed_at FROM apply_token_account_owner_cap_policy($1,$2,$3,$4,$5)`, max, expectedValue, key, reason, requestID).Scan(&version, &returnedMax, &configDigest, &requestDigest, &event, &changedBy, &changedAt)
		if err == nil && (returnedMax != max || configDigest != tokenAccountOwnerCapConfigDigest(int64(max), version) || requestDigest != tokenAccountOwnerCapRequestDigest(max, expected, reason) || changedBy != "token_policy_operator" || changedAt.IsZero()) {
			err = fmt.Errorf("invalid policy response version=%d max=%d digest=%s actor=%s", version, returnedMax, configDigest, changedBy)
		}
		return
	}

	requestID := uuid.New()
	version, event, err := apply(20, nil, "owner-cap-bootstrap-0001", "bootstrap production owner cap", requestID)
	if err != nil || version != 1 || event == uuid.Nil {
		t.Fatalf("bootstrap version/event=%d/%s err=%v", version, event, err)
	}
	replayVersion, replayEvent, err := apply(20, nil, "owner-cap-bootstrap-0001", "bootstrap production owner cap", requestID)
	if err != nil || replayVersion != version || replayEvent != event {
		t.Fatalf("bootstrap replay=%d/%s err=%v", replayVersion, replayEvent, err)
	}
	if _, _, err = apply(21, nil, "owner-cap-bootstrap-0001", "bootstrap production owner cap", requestID); err == nil || !strings.Contains(err.Error(), "IDEMPOTENCY_KEY_REUSED") {
		t.Fatalf("same key different payload err=%v", err)
	}
	if _, _, err = apply(21, &version, "owner-cap-nonnfc-0001", "change cafe\u0301 owner cap", uuid.New()); err == nil || !strings.Contains(err.Error(), "TOKEN_ACCOUNT_POLICY_CONFIG_MISMATCH") {
		t.Fatalf("non-NFC policy reason err=%v", err)
	}
	var replayGroup sync.WaitGroup
	replayOutcomes := make(chan struct {
		version int64
		event   uuid.UUID
		err     error
	}, 2)
	for range 2 {
		replayGroup.Add(1)
		go func() {
			defer replayGroup.Done()
			gotVersion, gotEvent, gotErr := apply(20, nil, "owner-cap-bootstrap-0001", "bootstrap production owner cap", requestID)
			replayOutcomes <- struct {
				version int64
				event   uuid.UUID
				err     error
			}{gotVersion, gotEvent, gotErr}
		}()
	}
	replayGroup.Wait()
	close(replayOutcomes)
	for outcome := range replayOutcomes {
		if outcome.err != nil || outcome.version != version || outcome.event != event {
			t.Fatalf("concurrent exact replay=%d/%s err=%v", outcome.version, outcome.event, outcome.err)
		}
	}

	expected := version
	results := make(chan error, 2)
	var group sync.WaitGroup
	for index := 0; index < 2; index++ {
		index := index
		group.Add(1)
		go func() {
			defer group.Done()
			_, _, applyErr := apply(30+index, &expected, fmt.Sprintf("owner-cap-cas-%04d-unique", index), "change owner cap after review", uuid.New())
			results <- applyErr
		}()
	}
	group.Wait()
	close(results)
	succeeded, conflicted := 0, 0
	for applyErr := range results {
		if applyErr == nil {
			succeeded++
		} else if strings.Contains(applyErr.Error(), "COMMAND_PRECHECK_CHANGED") || strings.Contains(applyErr.Error(), "40001") {
			conflicted++
		} else {
			t.Fatalf("unexpected CAS error: %v", applyErr)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("CAS outcomes success/conflict=%d/%d", succeeded, conflicted)
	}
	var revisions, outbox int
	if err = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM token_account_owner_cap_policy_revisions),(SELECT count(*) FROM outbox_events WHERE event_type='token.account-owner-cap-policy.changed.v1')`).Scan(&revisions, &outbox); err != nil || revisions != 2 || outbox != 2 {
		t.Fatalf("policy facts revisions/outbox=%d/%d err=%v", revisions, outbox, err)
	}
	capacity, err := loadTokenAccountOwnerCapacity(ctx, pool, "USER", uuid.New())
	if err != nil || capacity.Policy.PolicyVersion != 2 || capacity.Policy.ConfigDigest != tokenAccountOwnerCapConfigDigest(capacity.Policy.MaxPerOwner, 2) {
		t.Fatalf("active policy=%+v err=%v", capacity, err)
	}

	var publicExecute, runtimeExecute, publicLockExecute, runtimeLockExecute, runtimeSelect, runtimeWrite, publicCreate, deployerCreate, runtimeCreate bool
	if err = pool.QueryRow(ctx, `SELECT
 has_function_privilege('public','apply_token_account_owner_cap_policy(integer,bigint,text,text,uuid)','EXECUTE'),
 has_function_privilege('token_gateway','apply_token_account_owner_cap_policy(integer,bigint,text,text,uuid)','EXECUTE'),
	 has_function_privilege('public','lock_token_account_owner_cap_policy()','EXECUTE'),
	 has_function_privilege('token_gateway','lock_token_account_owner_cap_policy()','EXECUTE'),
		has_table_privilege('token_gateway','token_account_owner_cap_policy_revisions','SELECT') AND has_table_privilege('token_gateway','token_account_owner_cap_policy_active','SELECT'),
		 has_table_privilege('token_gateway','token_account_owner_cap_policy_revisions','INSERT,UPDATE,DELETE') OR has_table_privilege('token_gateway','token_account_owner_cap_policy_active','INSERT,UPDATE,DELETE'),
 has_schema_privilege('public','public','CREATE'),
		 has_schema_privilege('token_cap_policy_deployer','public','CREATE'),has_schema_privilege('token_gateway','public','CREATE')`).Scan(&publicExecute, &runtimeExecute, &publicLockExecute, &runtimeLockExecute, &runtimeSelect, &runtimeWrite, &publicCreate, &deployerCreate, &runtimeCreate); err != nil || publicExecute || runtimeExecute || publicLockExecute || !runtimeLockExecute || !runtimeSelect || runtimeWrite || publicCreate || deployerCreate || runtimeCreate {
		t.Fatalf("policy ACL apply public/runtime=%t/%t lock public/runtime=%t/%t runtime select/write=%t/%t public/deployer create=%t/%t err=%v", publicExecute, runtimeExecute, publicLockExecute, runtimeLockExecute, runtimeSelect, runtimeWrite, publicCreate, deployerCreate, err)
	}
	var lockedVersion int64
	if err = pool.QueryRow(ctx, `SELECT lock_token_account_owner_cap_policy()`).Scan(&lockedVersion); err != nil || lockedVersion != 2 {
		t.Fatalf("runtime fixed policy lock version=%d err=%v", lockedVersion, err)
	}
	var permissionError *pgconn.PgError
	if err = pool.QueryRow(ctx, `SELECT active_policy_version FROM token_account_owner_cap_policy_active WHERE singleton_key FOR SHARE`).Scan(&lockedVersion); !errors.As(err, &permissionError) || permissionError.Code != "42501" {
		t.Fatalf("runtime direct policy row lock must remain denied: %v", err)
	}
	var policyTableOwner, policySequenceOwner, applyFunctionOwner, lockFunctionOwner string
	if err = pool.QueryRow(ctx, `SELECT
 pg_get_userbyid((SELECT relowner FROM pg_class WHERE oid='token_account_owner_cap_policy_revisions'::regclass)),
	 pg_get_userbyid((SELECT relowner FROM pg_class WHERE oid='token_account_owner_cap_policy_revisions_policy_version_seq'::regclass)),
	 pg_get_userbyid((SELECT proowner FROM pg_proc WHERE oid='apply_token_account_owner_cap_policy(integer,bigint,text,text,uuid)'::regprocedure)),
	 pg_get_userbyid((SELECT proowner FROM pg_proc WHERE oid='lock_token_account_owner_cap_policy()'::regprocedure))`).Scan(&policyTableOwner, &policySequenceOwner, &applyFunctionOwner, &lockFunctionOwner); err != nil || policyTableOwner != "token_cap_policy_deployer" || policySequenceOwner != "token_cap_policy_deployer" || applyFunctionOwner != "token_cap_policy_deployer" || lockFunctionOwner != "token_cap_policy_deployer" {
		t.Fatalf("policy owners table/sequence/apply/lock=%s/%s/%s/%s err=%v", policyTableOwner, policySequenceOwner, applyFunctionOwner, lockFunctionOwner, err)
	}
	var functionConfig []string
	if err = pool.QueryRow(ctx, `SELECT proconfig FROM pg_proc WHERE oid='apply_token_account_owner_cap_policy(integer,bigint,text,text,uuid)'::regprocedure`).Scan(&functionConfig); err != nil || !containsString(functionConfig, "search_path=pg_catalog") {
		t.Fatalf("policy function search_path=%v err=%v", functionConfig, err)
	}
	if err = pool.QueryRow(ctx, `SELECT proconfig FROM pg_proc WHERE oid='lock_token_account_owner_cap_policy()'::regprocedure`).Scan(&functionConfig); err != nil || !containsString(functionConfig, "search_path=pg_catalog") {
		t.Fatalf("policy lock function search_path=%v err=%v", functionConfig, err)
	}
	permissionError = nil
	if _, err = pool.Exec(ctx, `CREATE TABLE public.runtime_shadow_policy(value integer)`); !errors.As(err, &permissionError) || permissionError.Code != "42501" {
		t.Fatalf("runtime schema CREATE must be denied: %v", err)
	}
	permissionError = nil
	if _, err = pool.Exec(ctx, `UPDATE token_account_owner_cap_policy_revisions SET max_per_owner=max_per_owner+1 WHERE policy_version=1`); !errors.As(err, &permissionError) || permissionError.Code != "42501" {
		t.Fatalf("immutable revision update err=%v", err)
	}

	if _, err = connection.Exec(ctx, string(downSQL)); err == nil || !strings.Contains(err.Error(), "keep migration 000029") {
		t.Fatalf("historical down did not fail closed: %v", err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM token_account_owner_cap_policy_revisions`).Scan(&revisions); err != nil || revisions != 2 {
		t.Fatalf("failed down destroyed history revisions=%d err=%v", revisions, err)
	}
}

func tokenAccountOwnerCapRequestDigest(max int, expected *int64, reason string) string {
	expectedText := "null"
	if expected != nil {
		expectedText = fmt.Sprintf("%d", *expected)
	}
	encodedReason, _ := json.Marshal(reason)
	digest := sha256.Sum256([]byte(fmt.Sprintf("max_per_owner=%d\nexpected_active_policy_version=%s\nreason=%s\n", max, expectedText, encodedReason)))
	return hex.EncodeToString(digest[:])
}

func assertPolicyTestIdentity(t *testing.T, ctx context.Context, query interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, database, session, current string) {
	t.Helper()
	var gotDatabase, gotSession, gotCurrent string
	var superuser bool
	err := query.QueryRow(ctx, `SELECT current_database(),session_user,current_user,(SELECT rolsuper FROM pg_roles WHERE rolname=session_user)`).Scan(&gotDatabase, &gotSession, &gotCurrent, &superuser)
	if err != nil || gotDatabase != database || gotSession != session || gotCurrent != current || superuser {
		t.Fatalf("policy test identity database/session/current/super=%s/%s/%s/%v want=%s/%s/%s err=%v", gotDatabase, gotSession, gotCurrent, superuser, database, session, current, err)
	}
}
