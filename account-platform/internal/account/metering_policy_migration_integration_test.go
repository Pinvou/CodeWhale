package account

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
)

func TestProtocolGovernanceMigrationMatchesRegisteredPermissions(t *testing.T) {
	repository := accountDatabase(t)
	for _, command := range []string{"TOKEN_PROTOCOL_DRAFT_CREATE", "TOKEN_PROTOCOL_PUBLISH", "TOKEN_FORMULA_DRAFT_CREATE", "TOKEN_FORMULA_PUBLISH"} {
		t.Run(command, func(t *testing.T) {
			policy, err := approvalpolicy.Lookup(command)
			if err != nil {
				t.Fatal(err)
			}
			var version, count, snapshotVersion int
			var mode, permission, risk, executor string
			var selfForbidden bool
			var fields []byte
			err = repository.pool.QueryRow(context.Background(), `SELECT policy_version,approval_mode,requester_permission,risk_class,approval_count,self_approval_forbidden,snapshot_schema_version,critical_resource_fields,executor_identity FROM approval_command_policies WHERE command_type=$1`, command).Scan(&version, &mode, &permission, &risk, &count, &selfForbidden, &snapshotVersion, &fields, &executor)
			if err != nil {
				t.Fatal(err)
			}
			var critical []string
			if err := json.Unmarshal(fields, &critical); err != nil {
				t.Fatal(err)
			}
			if version != approvalpolicy.Version || mode != string(policy.Mode) || permission != policy.RequesterPermission || risk != policy.RiskClass || count != policy.ApprovalCount || selfForbidden != policy.SelfApprovalForbidden || snapshotVersion != policy.SnapshotSchemaVersion || executor != policy.ExecutorIdentity || !slices.Equal(critical, policy.CriticalResourceFields) {
				t.Fatalf("migrated policy differs from runtime registration for %s", command)
			}
			if _, err := repository.pool.Exec(context.Background(), `UPDATE approval_command_policies SET requester_permission='platform.roles.assign' WHERE command_type=$1`, command); err == nil {
				t.Fatal("policy facts must remain immutable after migration")
			}
		})
	}
}
