package token

import (
	"slices"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
)

// The current operation is governed by v3. A target command retains its own
// frozen v2/v3 policy version; changing this gate never rewrites old results or
// conflates command policy with the independently versioned owner cap policy.
func validTokenAccountRecoveryPolicy(policy approvalpolicy.Policy) bool {
	return policy.CommandType == "TOKEN_ACCOUNT_CREATE" && policy.PolicyVersion == 3 && policy.SnapshotSchemaVersion == 3 &&
		policy.Mode == approvalpolicy.AuditOnly && policy.RequesterPermission == "platform.tokens.manage" && policy.ApproverPermission == "" &&
		policy.RiskClass == "GOVERNANCE_OR_NORMAL_STATE_MACHINE" && policy.ApprovalCount == 0 && !policy.SelfApprovalForbidden &&
		policy.ExpiresAfter == 24*time.Hour && policy.ExecutionWindow == 5*time.Minute && policy.ExecutorIdentity == "requesting-domain-service" &&
		slices.Equal(policy.CriticalResourceFields, tokenCommandSpecs["TOKEN_ACCOUNT_CREATE"].RequiredFacts)
}
