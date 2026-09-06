package approvalpolicy

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var accountCreateV3Fields = []string{"owner_type", "owner_id", "owner_authorization_version", "owner_version", "owner_identity_digest", "owner_account_count", "owner_account_cap", "owner_account_cap_policy_version", "creation_version", "precheck_fingerprint", "request_fingerprint"}

func TestTokenAccountCreateV3AndUnchangedStatusPolicy(t *testing.T) {
	create, err := Lookup("TOKEN_ACCOUNT_CREATE")
	if err != nil || create.PolicyVersion != 3 || create.SnapshotSchemaVersion != 3 || !reflect.DeepEqual(create.CriticalResourceFields, accountCreateV3Fields) || create.Mode != AuditOnly {
		t.Fatalf("create v3 does not describe the complete eleven-field contract: %#v %v", create, err)
	}
	status, err := Lookup("TOKEN_ACCOUNT_STATUS_CHANGE")
	if err != nil || status.PolicyVersion != 2 || status.SnapshotSchemaVersion != 2 || !reflect.DeepEqual(status.CriticalResourceFields, []string{"token_account_id", "expected_account_version", "target_status", "precheck_fingerprint", "request_fingerprint"}) {
		t.Fatalf("already correct status policy must remain v2: %#v %v", status, err)
	}
}

func TestTokenAccountCreateV3PolicyPostgresIntegration(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if dsn == "" {
		t.Skip("PINVOU_TEST_ACCOUNT_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	loaded, err := migrations.Load("../../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	var before, through []migrations.Migration
	for _, migration := range loaded {
		if migration.Version < 48 {
			before = append(before, migration)
		}
		if migration.Version <= 48 {
			through = append(through, migration)
		}
	}
	if len(through) != len(before)+1 || through[len(through)-1].Version != 48 {
		t.Fatal("the independent Account 000048 migration is required")
	}
	fixture := func(t *testing.T) *pgx.Conn {
		connection, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		schema := "account_create_v3_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		quoted := pgx.Identifier{schema}.Sanitize()
		if _, err = connection.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			migrations.Unlock(context.Background(), connection)
			_, _ = connection.Exec(context.Background(), "DROP SCHEMA "+quoted+" CASCADE")
			_ = connection.Close(context.Background())
		})
		if _, err = connection.Exec(ctx, "SET search_path TO "+quoted); err != nil {
			t.Fatal(err)
		}
		if err = migrations.Up(ctx, connection, before); err != nil {
			t.Fatal(err)
		}
		migrations.Unlock(ctx, connection)
		return connection
	}
	rowJSON := func(t *testing.T, connection *pgx.Conn, command string, version int64) string {
		var raw string
		if err := connection.QueryRow(ctx, `SELECT to_jsonb(p)::text FROM approval_command_policies p WHERE command_type=$1 AND policy_version=$2`, command, version).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	t.Run("forward_and_down_preserve_history", func(t *testing.T) {
		connection := fixture(t)
		old := rowJSON(t, connection, "TOKEN_ACCOUNT_CREATE", 2)
		oldStatus := rowJSON(t, connection, "TOKEN_ACCOUNT_STATUS_CHANGE", 2)
		for count := 0; count < 2; count++ {
			if err := migrations.Up(ctx, connection, through); err != nil {
				t.Fatal(err)
			}
			migrations.Unlock(ctx, connection)
		}
		var actual Policy
		var mode string
		var fields json.RawMessage
		var expires, window int64
		if err := connection.QueryRow(ctx, `SELECT command_type,policy_version,approval_mode,requester_permission,coalesce(approver_permission,''),risk_class,approval_count,self_approval_forbidden,snapshot_schema_version,critical_resource_fields,extract(epoch FROM expires_after)::bigint,extract(epoch FROM execution_window)::bigint,executor_identity FROM approval_command_policies WHERE command_type='TOKEN_ACCOUNT_CREATE' AND policy_version=3`).Scan(&actual.CommandType, &actual.PolicyVersion, &mode, &actual.RequesterPermission, &actual.ApproverPermission, &actual.RiskClass, &actual.ApprovalCount, &actual.SelfApprovalForbidden, &actual.SnapshotSchemaVersion, &fields, &expires, &window, &actual.ExecutorIdentity); err != nil {
			t.Fatal(err)
		}
		actual.Mode = Mode(mode)
		actual.ExpiresAfter = time.Duration(expires) * time.Second
		actual.ExecutionWindow = time.Duration(window) * time.Second
		if json.Unmarshal(fields, &actual.CriticalResourceFields) != nil {
			t.Fatal("invalid policy fields")
		}
		expected, _ := Lookup("TOKEN_ACCOUNT_CREATE")
		if !reflect.DeepEqual(expected, actual) {
			t.Fatalf("v3 database/registry mismatch: %#v", actual)
		}
		current := rowJSON(t, connection, "TOKEN_ACCOUNT_CREATE", 3)
		if old != rowJSON(t, connection, "TOKEN_ACCOUNT_CREATE", 2) || oldStatus != rowJSON(t, connection, "TOKEN_ACCOUNT_STATUS_CHANGE", 2) {
			t.Fatal("existing v2 facts were rewritten")
		}
		if _, err := connection.Exec(ctx, `UPDATE approval_command_policies SET risk_class='CHANGED' WHERE command_type='TOKEN_ACCOUNT_CREATE' AND policy_version=3`); err == nil {
			t.Fatal("new policy version must be immutable")
		}
		if err := migrations.DownOne(ctx, connection, through); err != nil {
			t.Fatal(err)
		}
		migrations.Unlock(ctx, connection)
		if old != rowJSON(t, connection, "TOKEN_ACCOUNT_CREATE", 2) || current != rowJSON(t, connection, "TOKEN_ACCOUNT_CREATE", 3) {
			t.Fatal("down must preserve cross-domain policy history")
		}
		if err := migrations.Up(ctx, connection, through); err != nil {
			t.Fatal(err)
		}
		migrations.Unlock(ctx, connection)
	})
	t.Run("existing_conflicting_v3_fails_closed", func(t *testing.T) {
		connection := fixture(t)
		if _, err := connection.Exec(ctx, `INSERT INTO approval_command_policies(command_type,policy_version,approval_mode,requester_permission,approver_permission,risk_class,approval_count,self_approval_forbidden,snapshot_schema_version,critical_resource_fields,expires_after,execution_window,executor_identity,created_at) SELECT command_type,3,approval_mode,requester_permission,approver_permission,risk_class,approval_count,self_approval_forbidden,3,critical_resource_fields,expires_after,execution_window,executor_identity,created_at FROM approval_command_policies WHERE command_type='TOKEN_ACCOUNT_CREATE' AND policy_version=2`); err != nil {
			t.Fatal(err)
		}
		conflict := rowJSON(t, connection, "TOKEN_ACCOUNT_CREATE", 3)
		if err := migrations.Up(ctx, connection, through); err == nil {
			t.Fatal("conflicting immutable policy must reject migration")
		}
		migrations.Unlock(ctx, connection)
		if conflict != rowJSON(t, connection, "TOKEN_ACCOUNT_CREATE", 3) {
			t.Fatal("migration must not repair conflict by overwriting history")
		}
	})
}
