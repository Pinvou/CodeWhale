package approvalpolicy

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/gin-gonic/gin"
)

type Mode string

const (
	ApprovalRequired Mode = "APPROVAL_REQUIRED"
	AuditOnly        Mode = "AUDIT_ONLY"
	Version               = 1
)

type Policy struct {
	CommandType            string        `json:"command_type"`
	PolicyVersion          int64         `json:"policy_version"`
	Mode                   Mode          `json:"approval_mode"`
	RequesterPermission    string        `json:"requester_permission"`
	ApproverPermission     string        `json:"approver_permission,omitempty"`
	RiskClass              string        `json:"risk_class"`
	ApprovalCount          int           `json:"approval_count"`
	SelfApprovalForbidden  bool          `json:"self_approval_forbidden"`
	SnapshotSchemaVersion  int           `json:"snapshot_schema_version"`
	CriticalResourceFields []string      `json:"critical_resource_fields"`
	ExpiresAfter           time.Duration `json:"expires_after_nanoseconds"`
	ExecutionWindow        time.Duration `json:"execution_window_nanoseconds"`
	ExecutorIdentity       string        `json:"executor_identity"`
}

var ErrPolicyNotRegistered = errors.New("management command policy is not registered")

var policies = buildPolicies()

func ValidateAll(commands map[string]Mode) error {
	for commandType, expected := range commands {
		policy, err := Lookup(commandType)
		if err != nil || policy.Mode != expected {
			return ErrPolicyNotRegistered
		}
	}
	return nil
}

func Lookup(commandType string) (Policy, error) {
	policy, ok := policies[strings.ToUpper(strings.TrimSpace(commandType))]
	if !ok {
		return Policy{}, ErrPolicyNotRegistered
	}
	return policy, nil
}

func All() []Policy {
	items := make([]Policy, 0, len(policies))
	for _, policy := range policies {
		items = append(items, policy)
	}
	return items
}

// Require annotates every management write route with its stable policy and
// fails closed on unknown or conflicting classifications. Approval-required
// commands cannot be invoked through their legacy direct-mutation route.
func Require(commandType string, expected Mode) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		policy, err := Lookup(commandType)
		if err != nil || policy.Mode != expected {
			api.WriteError(ctx.Writer, http.StatusServiceUnavailable, "COMMAND_POLICY_UNAVAILABLE", "management command policy is unavailable", httpserver.RequestID(ctx.Request.Context()))
			ctx.Abort()
			return
		}
		if expected == ApprovalRequired {
			api.WriteError(ctx.Writer, http.StatusConflict, "APPROVAL_WORKFLOW_REQUIRED", "submit this command through the approval workflow", httpserver.RequestID(ctx.Request.Context()))
			ctx.Abort()
			return
		}
		ctx.Header("X-Command-Type", policy.CommandType)
		ctx.Header("X-Command-Policy-Version", fmt.Sprint(policy.PolicyVersion))
		ctx.Next()
	}
}

func buildPolicies() map[string]Policy {
	result := make(map[string]Policy)
	for commandType, permission := range map[string]string{
		"ACCOUNT_USER_STATUS_CHANGE":       "platform.users.status.manage",
		"ACCOUNT_REGISTRATION_RETRY":       "platform.registrations.repair",
		"ACCOUNT_REGISTRATION_COMPENSATE":  "platform.registrations.repair",
		"ACCOUNT_PROFILE_CHANGE_RETRY":     "platform.profile-changes.repair",
		"ACCOUNT_SMS_CONFIGURATION_UPDATE": "platform.sms-config.manage",
	} {
		result[commandType] = Policy{CommandType: commandType, PolicyVersion: Version, Mode: AuditOnly,
			RequesterPermission: permission, RiskClass: "GOVERNANCE_OR_NORMAL_STATE_MACHINE",
			ApprovalCount: 0, SelfApprovalForbidden: false, SnapshotSchemaVersion: 1,
			CriticalResourceFields: []string{"target_id", "request_fingerprint"},
			ExpiresAfter:           24 * time.Hour, ExecutionWindow: 5 * time.Minute, ExecutorIdentity: "requesting-domain-service"}
	}
	return result
}
