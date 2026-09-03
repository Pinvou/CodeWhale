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
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrations.Up(ctx, connection, loaded); err != nil {
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
	if err := migrations.DownOne(ctx, connection, loaded); err != nil {
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
