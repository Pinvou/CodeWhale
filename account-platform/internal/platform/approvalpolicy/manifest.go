package approvalpolicy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// governancePolicyCommands is a closed database-backed policy contract, not
// the intersection of whatever happens to exist in the current database.
// Workload-only AUDIT_ONLY registry entries have no approval database row.
var governancePolicyCommands = []string{
	"WALLET_ADJUSTMENT", "PAYMENT_PROVIDER_RESUME", "PAYMENT_PRODUCT_RESUME",
	"TOKEN_PROTOCOL_DRAFT_CREATE",
	"TOKEN_PROTOCOL_PUBLISH",
	"TOKEN_FORMULA_DRAFT_CREATE",
	"TOKEN_FORMULA_PUBLISH",
	"TOKEN_MODEL_DRAFT_SAVE",
	"TOKEN_MODEL_PUBLISH",
	"TOKEN_MODEL_LIFECYCLE_CHANGE",
	"TOKEN_MODEL_CATALOG_SYNC",
	"TOKEN_MODEL_CATALOG_ROLLBACK",
	"TOKEN_ACCOUNT_CREATE",
	"TOKEN_ACCOUNT_STATUS_CHANGE",
	"TOKEN_ROUTE_UPSERT",
	"TOKEN_ROUTE_DELETE",
	"TOKEN_CREDENTIAL_ENROLL",
	"TOKEN_CREDENTIAL_ROTATE",
	"TOKEN_CREDENTIAL_REVOKE",
	"TOKEN_CREDENTIAL_VALIDATE",
	"TOKEN_ROUTE_PROVISIONING_REPAIR",
	"TOKEN_USAGE_RECONCILIATION_RETRY",
}

func governancePolicies() ([]Policy, error) {
	result := make([]Policy, 0, len(governancePolicyCommands))
	for _, command := range governancePolicyCommands {
		policy, err := Lookup(command)
		if err != nil {
			return nil, err
		}
		result = append(result, policy)
	}
	slices.SortFunc(result, func(a, b Policy) int { return strings.Compare(a.CommandType, b.CommandType) })
	return result, nil
}

// PolicySetDigest binds the three installed executable Billing policies. Historical database versions remain immutable.
func PolicySetDigest() (string, error) {
	policies, err := governancePolicies()
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(policies)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

type PolicyQuery interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// VerifyPolicySet must run against the authoritative Account database in a
// single caller-owned snapshot. It neither changes policy facts nor starts a
// nested transaction. Errors deliberately identify only the public command.
func VerifyPolicySet(ctx context.Context, query PolicyQuery) (string, error) {
	if query == nil {
		return "", fmt.Errorf("policy database is unavailable")
	}
	policies, err := governancePolicies()
	if err != nil {
		return "", err
	}
	for _, expected := range policies {
		var actual Policy
		var mode string
		var critical json.RawMessage
		var exactExpiry, exactWindow bool
		err := query.QueryRow(ctx, `SELECT command_type,policy_version,approval_mode,requester_permission,coalesce(approver_permission,''),risk_class,approval_count,self_approval_forbidden,snapshot_schema_version,critical_resource_fields,expires_after=($2::bigint*interval '1 microsecond'),execution_window=($3::bigint*interval '1 microsecond'),executor_identity FROM approval_command_policies WHERE command_type=$1 ORDER BY policy_version DESC LIMIT 1`, expected.CommandType, expected.ExpiresAfter.Microseconds(), expected.ExecutionWindow.Microseconds()).Scan(
			&actual.CommandType, &actual.PolicyVersion, &mode, &actual.RequesterPermission, &actual.ApproverPermission, &actual.RiskClass, &actual.ApprovalCount, &actual.SelfApprovalForbidden, &actual.SnapshotSchemaVersion, &critical, &exactExpiry, &exactWindow, &actual.ExecutorIdentity,
		)
		if err != nil || json.Unmarshal(critical, &actual.CriticalResourceFields) != nil {
			return "", fmt.Errorf("policy evidence unavailable for %s", expected.CommandType)
		}
		actual.Mode = Mode(mode)
		// Equality is checked by PostgreSQL without integer-second rounding.
		actual.ExpiresAfter = expected.ExpiresAfter
		actual.ExecutionWindow = expected.ExecutionWindow
		if !exactExpiry || !exactWindow || !reflect.DeepEqual(expected, actual) {
			return "", fmt.Errorf("policy evidence differs for %s", expected.CommandType)
		}
	}
	return PolicySetDigest()
}
