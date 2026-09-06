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

var routeUpsertV3Fields = []string{"token_account_id", "expected_account_version", "expected_route_version", "model_id", "protocol_version_id", "precheck_fingerprint", "request_fingerprint"}

func TestTokenRouteUpsertV3Policy(t *testing.T) {
	policy, err := Lookup("TOKEN_ROUTE_UPSERT")
	if err != nil || policy.PolicyVersion != 3 || policy.SnapshotSchemaVersion != 3 || !reflect.DeepEqual(policy.CriticalResourceFields, routeUpsertV3Fields) || policy.Mode != AuditOnly {
		t.Fatalf("route upsert v3 does not describe the complete seven-field contract: %#v %v", policy, err)
	}
}

func TestTokenRouteUpsertV3PolicyPostgresIntegration(t *testing.T) {
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
		if migration.Version < 49 {
			before = append(before, migration)
		}
		if migration.Version <= 49 {
			through = append(through, migration)
		}
	}
	if len(through) != len(before)+1 || through[len(through)-1].Version != 49 {
		t.Fatal("the independent Account 000049 migration is required")
	}
	fixture := func(t *testing.T) *pgx.Conn {
		connection, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		schema := "route_upsert_v3_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
		old := rowJSON(t, connection, "TOKEN_ROUTE_UPSERT", 2)
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
		if err := connection.QueryRow(ctx, `SELECT command_type,policy_version,approval_mode,requester_permission,coalesce(approver_permission,''),risk_class,approval_count,self_approval_forbidden,snapshot_schema_version,critical_resource_fields,extract(epoch FROM expires_after)::bigint,extract(epoch FROM execution_window)::bigint,executor_identity FROM approval_command_policies WHERE command_type='TOKEN_ROUTE_UPSERT' AND policy_version=3`).Scan(&actual.CommandType, &actual.PolicyVersion, &mode, &actual.RequesterPermission, &actual.ApproverPermission, &actual.RiskClass, &actual.ApprovalCount, &actual.SelfApprovalForbidden, &actual.SnapshotSchemaVersion, &fields, &expires, &window, &actual.ExecutorIdentity); err != nil {
			t.Fatal(err)
		}
		actual.Mode = Mode(mode)
		actual.ExpiresAfter = time.Duration(expires) * time.Second
		actual.ExecutionWindow = time.Duration(window) * time.Second
		if json.Unmarshal(fields, &actual.CriticalResourceFields) != nil {
			t.Fatal("invalid policy fields")
		}
		expected, _ := Lookup("TOKEN_ROUTE_UPSERT")
		if !reflect.DeepEqual(expected, actual) {
			t.Fatalf("v3 database/registry mismatch: %#v", actual)
		}
		current := rowJSON(t, connection, "TOKEN_ROUTE_UPSERT", 3)
		if old != rowJSON(t, connection, "TOKEN_ROUTE_UPSERT", 2) {
			t.Fatal("existing v2 facts were rewritten")
		}
		if _, err := connection.Exec(ctx, `UPDATE approval_command_policies SET risk_class='CHANGED' WHERE command_type='TOKEN_ROUTE_UPSERT' AND policy_version=3`); err == nil {
			t.Fatal("new policy version must be immutable")
		}
		if err := migrations.DownOne(ctx, connection, through); err != nil {
			t.Fatal(err)
		}
		migrations.Unlock(ctx, connection)
		if old != rowJSON(t, connection, "TOKEN_ROUTE_UPSERT", 2) || current != rowJSON(t, connection, "TOKEN_ROUTE_UPSERT", 3) {
			t.Fatal("down must preserve cross-domain policy history")
		}
		if err := migrations.Up(ctx, connection, through); err != nil {
			t.Fatal(err)
		}
		migrations.Unlock(ctx, connection)
	})
	t.Run("existing_conflicting_v3_fails_closed", func(t *testing.T) {
		connection := fixture(t)
		if _, err := connection.Exec(ctx, `INSERT INTO approval_command_policies(command_type,policy_version,approval_mode,requester_permission,approver_permission,risk_class,approval_count,self_approval_forbidden,snapshot_schema_version,critical_resource_fields,expires_after,execution_window,executor_identity,created_at) SELECT command_type,3,approval_mode,requester_permission,approver_permission,risk_class,approval_count,self_approval_forbidden,3,critical_resource_fields,expires_after,execution_window,executor_identity,created_at FROM approval_command_policies WHERE command_type='TOKEN_ROUTE_UPSERT' AND policy_version=2`); err != nil {
			t.Fatal(err)
		}
		conflict := rowJSON(t, connection, "TOKEN_ROUTE_UPSERT", 3)
		if err := migrations.Up(ctx, connection, through); err == nil {
			t.Fatal("conflicting immutable policy must reject migration")
		}
		migrations.Unlock(ctx, connection)
		if conflict != rowJSON(t, connection, "TOKEN_ROUTE_UPSERT", 3) {
			t.Fatal("migration must not repair conflict by overwriting history")
		}
	})
}
