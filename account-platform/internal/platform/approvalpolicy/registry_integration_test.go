package approvalpolicy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestTokenProtocolFormulaPoliciesPostgresIntegration compares the executable
// registry with the real Account migration chain. CI supplies an isolated
// database and rejects SKIP.
func TestTokenProtocolFormulaPoliciesPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_ACCOUNT_DATABASE_URL is not configured")
	}
	if testing.Short() && os.Getenv("PINVOU_TEST_ACCOUNT_POLICY_GATE") != "1" {
		t.Skip("Account command-policy PostgreSQL gate is run explicitly before the short race suite")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close(context.Background()) })
	schema := "token_command_policy_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := connection.Exec(ctx, `CREATE SCHEMA `+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = connection.Exec(context.Background(), `DROP SCHEMA `+identifier+` CASCADE`) })
	if _, err := connection.Exec(ctx, `SET search_path TO `+identifier); err != nil {
		t.Fatal(err)
	}
	loaded, err := migrations.Load("../../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	protocolMigrations := accountPolicyMigrationsThrough(t, loaded, 44)
	if err := migrations.Up(ctx, connection, protocolMigrations); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)

	commands := []string{"TOKEN_PROTOCOL_DRAFT_CREATE", "TOKEN_PROTOCOL_PUBLISH", "TOKEN_FORMULA_DRAFT_CREATE", "TOKEN_FORMULA_PUBLISH"}
	for _, commandType := range commands {
		expected, err := Lookup(commandType)
		if err != nil {
			t.Fatal(err)
		}
		var actual Policy
		var mode string
		var critical []byte
		var expiresSeconds, executionSeconds int64
		err = connection.QueryRow(ctx, `SELECT command_type,policy_version,approval_mode,requester_permission,COALESCE(approver_permission,''),risk_class,approval_count,self_approval_forbidden,snapshot_schema_version,critical_resource_fields,EXTRACT(EPOCH FROM expires_after)::bigint,EXTRACT(EPOCH FROM execution_window)::bigint,executor_identity FROM approval_command_policies WHERE command_type=$1 AND policy_version=1`, commandType).Scan(
			&actual.CommandType, &actual.PolicyVersion, &mode, &actual.RequesterPermission, &actual.ApproverPermission, &actual.RiskClass, &actual.ApprovalCount, &actual.SelfApprovalForbidden, &actual.SnapshotSchemaVersion, &critical, &expiresSeconds, &executionSeconds, &actual.ExecutorIdentity,
		)
		if err != nil {
			t.Fatal(err)
		}
		actual.Mode = Mode(mode)
		actual.ExpiresAfter = time.Duration(expiresSeconds) * time.Second
		actual.ExecutionWindow = time.Duration(executionSeconds) * time.Second
		if err := json.Unmarshal(critical, &actual.CriticalResourceFields); err != nil {
			t.Fatal(err)
		}
		if !policiesEqual(expected, actual) {
			t.Fatalf("database and registry policy differ for %s:\nexpected=%#v\nactual=%#v", commandType, expected, actual)
		}
	}
	if _, err := connection.Exec(ctx, `UPDATE approval_command_policies SET risk_class='CHANGED' WHERE command_type='TOKEN_PROTOCOL_PUBLISH' AND policy_version=1`); err == nil {
		t.Fatal("installed policy versions must be immutable")
	}
	// Exercise the owning migration, not whichever forward migration happens
	// to be last when this regression test is run.
	if err := migrations.DownOne(ctx, connection, protocolMigrations); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := connection.QueryRow(ctx, `SELECT count(*) FROM approval_command_policies WHERE command_type=ANY($1)`, commands).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("down migration must remove only its unreferenced policy versions: count=%d err=%v", remaining, err)
	}
	if err := migrations.Up(ctx, connection, loaded); err != nil {
		t.Fatal(err)
	}
}

// TestTokenCommandGovernancePoliciesPostgresIntegration proves that the real
// 000045 migration matches the nineteen-command registry and invalidates one
// platform authorization snapshot once even when both allowed roles are held.
func TestTokenCommandGovernancePoliciesPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_ACCOUNT_DATABASE_URL is not configured")
	}
	if testing.Short() && os.Getenv("PINVOU_TEST_ACCOUNT_POLICY_GATE") != "1" {
		t.Skip("Account command-policy PostgreSQL gate is run explicitly before the short race suite")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close(context.Background()) })
	schema := "token_governance_policy_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := connection.Exec(ctx, `CREATE SCHEMA `+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = connection.Exec(context.Background(), `DROP SCHEMA `+identifier+` CASCADE`) })
	if _, err := connection.Exec(ctx, `SET search_path TO `+identifier); err != nil {
		t.Fatal(err)
	}
	loaded, err := migrations.Load("../../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	governanceMigrations := accountPolicyMigrationsThrough(t, loaded, 45)
	if err := migrations.Up(ctx, connection, accountPolicyMigrationsThrough(t, loaded, 44)); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)
	actor := uuid.New()
	requesterBinding := uuid.New()
	now := time.Now().UTC()
	if _, err := connection.Exec(ctx, `INSERT INTO users(id,status,display_name,user_code,created_at,updated_at) VALUES($1,'ACTIVE','governance actor',generate_unique_user_code(),$2,$2)`, actor, now); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,source,grant_source,starts_at,created_at) VALUES($1,$3,'PLATFORM_SUPER_ADMIN','INTEGRATION_TEST','INTEGRATION_TEST',$4,$4),($2,$3,'PLATFORM_MODEL_ADMIN','INTEGRATION_TEST','INTEGRATION_TEST',$4,$4)`, requesterBinding, uuid.New(), actor, now); err != nil {
		t.Fatal(err)
	}
	var before int64
	if err := connection.QueryRow(ctx, `SELECT version FROM authorization_versions WHERE user_id=$1 AND scope='PLATFORM'`, actor).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Up(ctx, connection, governanceMigrations); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)
	var after int64
	if err := connection.QueryRow(ctx, `SELECT version FROM authorization_versions WHERE user_id=$1 AND scope='PLATFORM'`, actor).Scan(&after); err != nil || after != before+1 {
		t.Fatalf("authorization version=%d want=%d err=%v", after, before+1, err)
	}
	// Subsequent migrations must preserve the nineteen installed policies.
	if err := migrations.Up(ctx, connection, loaded); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)
	approvalRequest := uuid.New()
	if _, err := connection.Exec(ctx, `INSERT INTO approval_requests(
		id,command_type,policy_version,requester_user_id,requester_binding_id,requester_authorization_version,
		idempotency_key,normalized_command,command_fingerprint,target_resource_type,target_resource_id,reason,
		redacted_evidence,expected_impact,status,expires_at,execution_deadline,submitted_at,decided_at,created_at,updated_at
	) VALUES($1,'TOKEN_MODEL_LIFECYCLE_CHANGE',2,$2,$3,$4,'migration-check-request','{}',decode(repeat('00',32),'hex'),
		'MODEL','model-for-error-class-check','migration constraint check','{}','{}','APPROVED',$5::timestamptz + INTERVAL '1 hour',
		$5::timestamptz + INTERVAL '5 minutes',$5,$5,$5,$5)`, approvalRequest, actor, requesterBinding, after, now); err != nil {
		t.Fatal(err)
	}
	invalidExecutions := []struct {
		name       string
		status     string
		errorClass any
	}{
		{name: "executing with error class", status: "EXECUTING", errorClass: "DOMAIN_REJECTED"},
		{name: "executed with error class", status: "EXECUTED", errorClass: "DOMAIN_REJECTED"},
		{name: "failed without error class", status: "FAILED", errorClass: nil},
	}
	for _, testCase := range invalidExecutions {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := connection.Exec(ctx, `INSERT INTO approval_executions(
				id,approval_request_id,command_type,command_fingerprint,execution_idempotency_key,executor_identity,
				job_run_id,status,authorization_snapshot,error_class,started_at
			) VALUES($1,$2,'TOKEN_MODEL_LIFECYCLE_CHANGE',decode(repeat('00',32),'hex'),$3,'token-command-executor',
				'migration-check-job',$4,'{}',$5,$6)`, uuid.New(), approvalRequest, "migration-check-"+strings.ReplaceAll(testCase.name, " ", "-"), testCase.status, testCase.errorClass, now)
			if err == nil || !strings.Contains(err.Error(), "approval_executions_stable_error_class") {
				t.Fatalf("expected stable error class constraint rejection, got %v", err)
			}
		})
	}
	critical := `{"catalog_version":7}`
	financial := `{"billing_price_revision":11,"price_set_fingerprint":"` + strings.Repeat("a", 64) + `"}`
	validExecution := uuid.New()
	if _, err := connection.Exec(ctx, `INSERT INTO approval_executions(id,approval_request_id,command_type,command_fingerprint,execution_idempotency_key,executor_identity,job_run_id,status,authorization_snapshot,critical_resource_versions,financial_snapshot,started_at) VALUES($1,$2,'TOKEN_MODEL_LIFECYCLE_CHANGE',decode(repeat('00',32),'hex'),$3,'token-command-executor','migration-valid-job','EXECUTING','{}',$4,$5,$6)`, validExecution, approvalRequest, "migration-valid-snapshot", critical, financial, now); err != nil {
		t.Fatal(err)
	}
	var frozenCritical, frozenFinancial []byte
	if err := connection.QueryRow(ctx, `SELECT critical_resource_versions,financial_snapshot FROM approval_executions WHERE id=$1`, validExecution).Scan(&frozenCritical, &frozenFinancial); err != nil || !strings.Contains(string(frozenCritical), `"catalog_version": 7`) || !strings.Contains(string(frozenFinancial), `"billing_price_revision": 11`) {
		t.Fatalf("frozen snapshots critical=%s financial=%s err=%v", frozenCritical, frozenFinancial, err)
	}
	if _, err := connection.Exec(ctx, `UPDATE approval_executions SET financial_snapshot='{}' WHERE id=$1`, validExecution); err == nil {
		t.Fatal("execution lease financial snapshot must be immutable")
	}
	// Test the snapshot-owning down inside a rolled-back transaction. A later
	// additive migration must not silently redirect this assertion to its down.
	snapshotMigrations := accountPolicyMigrationsThrough(t, loaded, 46)
	rollbackTx, err := connection.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, rollbackErr := rollbackTx.Exec(ctx, snapshotMigrations[len(snapshotMigrations)-1].DownSQL)
	_ = rollbackTx.Rollback(ctx)
	var postgresErr *pgconn.PgError
	if !errors.As(rollbackErr, &postgresErr) || postgresErr.Code != "55000" {
		t.Fatalf("rollback with execution history must fail closed: %v", rollbackErr)
	}
	if err := connection.QueryRow(ctx, `SELECT critical_resource_versions,financial_snapshot FROM approval_executions WHERE id=$1`, validExecution).Scan(&frozenCritical, &frozenFinancial); err != nil || len(frozenCritical) == 0 || len(frozenFinancial) == 0 {
		t.Fatalf("failed rollback removed frozen execution history: critical=%s financial=%s err=%v", frozenCritical, frozenFinancial, err)
	}
	if err := migrations.Up(ctx, connection, loaded); err != nil {
		t.Fatal(err)
	}
	var repeated int64
	if err := connection.QueryRow(ctx, `SELECT version FROM authorization_versions WHERE user_id=$1 AND scope='PLATFORM'`, actor).Scan(&repeated); err != nil || repeated != after {
		t.Fatalf("idempotent up changed authorization version=%d want=%d err=%v", repeated, after, err)
	}

	for commandType := range tokenGovernancePolicyTypes() {
		expected, err := Lookup(commandType)
		if err != nil {
			t.Fatal(err)
		}
		var actual Policy
		var mode string
		var critical []byte
		var expiresSeconds, executionSeconds int64
		err = connection.QueryRow(ctx, `SELECT command_type,policy_version,approval_mode,requester_permission,COALESCE(approver_permission,''),risk_class,approval_count,self_approval_forbidden,snapshot_schema_version,critical_resource_fields,EXTRACT(EPOCH FROM expires_after)::bigint,EXTRACT(EPOCH FROM execution_window)::bigint,executor_identity FROM approval_command_policies WHERE command_type=$1 AND policy_version=$2`, commandType, expected.PolicyVersion).Scan(&actual.CommandType, &actual.PolicyVersion, &mode, &actual.RequesterPermission, &actual.ApproverPermission, &actual.RiskClass, &actual.ApprovalCount, &actual.SelfApprovalForbidden, &actual.SnapshotSchemaVersion, &critical, &expiresSeconds, &executionSeconds, &actual.ExecutorIdentity)
		if err != nil {
			t.Fatal(err)
		}
		actual.Mode = Mode(mode)
		actual.ExpiresAfter = time.Duration(expiresSeconds) * time.Second
		actual.ExecutionWindow = time.Duration(executionSeconds) * time.Second
		if err := json.Unmarshal(critical, &actual.CriticalResourceFields); err != nil || !policiesEqual(expected, actual) {
			t.Fatalf("database and registry policy differ for %s: expected=%#v actual=%#v err=%v", commandType, expected, actual, err)
		}
	}
	var exactEdges, unexpectedEdges int
	if err := connection.QueryRow(ctx, `SELECT count(*) FROM role_permissions WHERE permission_code IN ('platform.models.lifecycle.request','platform.models.lifecycle.approve','platform.models.catalog.rollback.request','platform.models.catalog.rollback.approve','platform.tokens.credentials.revoke.request','platform.tokens.credentials.revoke.approve') AND role_code IN ('PLATFORM_SUPER_ADMIN','PLATFORM_MODEL_ADMIN')`).Scan(&exactEdges); err != nil || exactEdges != 12 {
		t.Fatalf("allowed role edges=%d want=12 err=%v", exactEdges, err)
	}
	if err := connection.QueryRow(ctx, `SELECT count(*) FROM role_permissions WHERE permission_code IN ('platform.models.lifecycle.request','platform.models.lifecycle.approve','platform.models.catalog.rollback.request','platform.models.catalog.rollback.approve','platform.tokens.credentials.revoke.request','platform.tokens.credentials.revoke.approve') AND role_code NOT IN ('PLATFORM_SUPER_ADMIN','PLATFORM_MODEL_ADMIN')`).Scan(&unexpectedEdges); err != nil || unexpectedEdges != 0 {
		t.Fatalf("unexpected role edges=%d err=%v", unexpectedEdges, err)
	}
	// Activation verifies the closed set of nineteen Token and three existing
	// executable Billing policies against one authoritative read-only snapshot.
	policyTx, err := connection.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	digest, verifyErr := VerifyPolicySet(ctx, policyTx)
	_ = policyTx.Rollback(ctx)
	if verifyErr != nil || len(digest) != 64 {
		t.Fatalf("activation full-policy database gate: digest=%q err=%v", digest, verifyErr)
	}
	// A session-local clone models corrupted evidence without ever mutating
	// the real immutable policy table or disabling its history triggers.
	if _, err := connection.Exec(ctx, `CREATE TEMP TABLE approval_command_policies AS TABLE `+identifier+`.approval_command_policies`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = connection.Exec(context.Background(), `DROP TABLE IF EXISTS pg_temp.approval_command_policies`)
	})
	for _, intervalColumn := range []string{"expires_after", "execution_window"} {
		t.Run("reject_subsecond_"+intervalColumn, func(t *testing.T) {
			column := pgx.Identifier{intervalColumn}.Sanitize()
			if _, err := connection.Exec(ctx, `UPDATE pg_temp.approval_command_policies SET `+column+`=`+column+`+interval '0.4 seconds' WHERE command_type='TOKEN_ROUTE_UPSERT' AND policy_version=3`); err != nil {
				t.Fatal(err)
			}
			if _, err := VerifyPolicySet(ctx, connection); err == nil {
				t.Fatalf("activation must reject a 400ms %s difference", intervalColumn)
			}
			if _, err := connection.Exec(ctx, `UPDATE pg_temp.approval_command_policies SET `+column+`=`+column+`-interval '0.4 seconds' WHERE command_type='TOKEN_ROUTE_UPSERT' AND policy_version=3`); err != nil {
				t.Fatal(err)
			}
			if restoredDigest, err := VerifyPolicySet(ctx, connection); err != nil || restoredDigest != digest {
				t.Fatalf("restored policy evidence must match exactly: digest=%q err=%v", restoredDigest, err)
			}
		})
	}
	if _, err := connection.Exec(ctx, `DROP TABLE pg_temp.approval_command_policies`); err != nil {
		t.Fatal(err)
	}
	if originalDigest, err := VerifyPolicySet(ctx, connection); err != nil || originalDigest != digest {
		t.Fatalf("real immutable policies changed during negative fixtures: digest=%q err=%v", originalDigest, err)
	}
}

func tokenGovernancePolicyTypes() map[string]struct{} {
	result := make(map[string]struct{}, 19)
	for _, commandType := range []string{
		"TOKEN_PROTOCOL_DRAFT_CREATE", "TOKEN_PROTOCOL_PUBLISH", "TOKEN_FORMULA_DRAFT_CREATE", "TOKEN_FORMULA_PUBLISH",
		"TOKEN_MODEL_DRAFT_SAVE", "TOKEN_MODEL_PUBLISH", "TOKEN_MODEL_LIFECYCLE_CHANGE", "TOKEN_MODEL_CATALOG_SYNC", "TOKEN_MODEL_CATALOG_ROLLBACK",
		"TOKEN_ACCOUNT_CREATE", "TOKEN_ACCOUNT_STATUS_CHANGE", "TOKEN_ROUTE_UPSERT", "TOKEN_ROUTE_DELETE",
		"TOKEN_CREDENTIAL_ENROLL", "TOKEN_CREDENTIAL_ROTATE", "TOKEN_CREDENTIAL_REVOKE", "TOKEN_CREDENTIAL_VALIDATE",
		"TOKEN_ROUTE_PROVISIONING_REPAIR", "TOKEN_USAGE_RECONCILIATION_RETRY",
	} {
		result[commandType] = struct{}{}
	}
	return result
}

func accountPolicyMigrationsThrough(t *testing.T, loaded []migrations.Migration, version int64) []migrations.Migration {
	t.Helper()
	for index, migration := range loaded {
		if migration.Version == version {
			return loaded[:index+1]
		}
	}
	t.Fatalf("required Account migration %d is missing", version)
	return nil
}

func policiesEqual(left, right Policy) bool {
	if left.CommandType != right.CommandType || left.PolicyVersion != right.PolicyVersion || left.Mode != right.Mode || left.RequesterPermission != right.RequesterPermission || left.ApproverPermission != right.ApproverPermission || left.RiskClass != right.RiskClass || left.ApprovalCount != right.ApprovalCount || left.SelfApprovalForbidden != right.SelfApprovalForbidden || left.SnapshotSchemaVersion != right.SnapshotSchemaVersion || left.ExpiresAfter != right.ExpiresAfter || left.ExecutionWindow != right.ExecutionWindow || left.ExecutorIdentity != right.ExecutorIdentity || len(left.CriticalResourceFields) != len(right.CriticalResourceFields) {
		return false
	}
	for index := range left.CriticalResourceFields {
		if left.CriticalResourceFields[index] != right.CriticalResourceFields[index] {
			return false
		}
	}
	return true
}
