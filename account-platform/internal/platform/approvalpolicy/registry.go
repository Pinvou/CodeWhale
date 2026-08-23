package approvalpolicy

import (
	"errors"
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
var executableApprovalCommands = map[string]struct{}{"WALLET_ADJUSTMENT": {}}

func buildPolicies() map[string]Policy {
	result := map[string]Policy{}
	financial := []string{
		"WALLET_ADJUSTMENT", "ENTITLEMENT_COMPONENT_ADJUSTMENT", "LEDGER_REPAIR", "RECEIVABLE_WRITE_OFF",
		"PAYMENT_FINANCIAL_REPAIR", "REFUND_EXCEPTION_REPAIR", "CHARGEBACK_FINANCIAL_REPAIR",
		"RECONCILIATION_FINANCIAL_REPAIR", "USAGE_SETTLEMENT_REPAIR",
	}
	for _, commandType := range financial {
		result[commandType] = Policy{
			CommandType: commandType, Mode: ApprovalRequired,
			RequesterPermission: "platform.billing.repairs.request", ApproverPermission: "platform.billing.repairs.approve",
			RiskClass: "FINANCIAL_FACT_CHANGE", ApprovalCount: 1, SelfApprovalForbidden: true,
			SnapshotSchemaVersion: 1, CriticalResourceFields: []string{"target_id", "resource_version", "financial_snapshot", "policy_version"},
			ExpiresAfter: 24 * time.Hour, ExecutionWindow: 5 * time.Minute, ExecutorIdentity: "billing-command-executor",
		}
	}
	auditCommands := map[string]string{
		"ACCOUNT_ROLE_BINDING_GRANT": "platform.roles.assign", "ACCOUNT_ROLE_BINDING_REVOKE": "platform.roles.assign",
		"ACCOUNT_USER_STATUS_CHANGE": "platform.users.status.manage", "ACCOUNT_USER_DELETE": "platform.users.status.manage",
		"ACCOUNT_ORGANIZATION_CREATE": "platform.organizations.create", "ACCOUNT_ORGANIZATION_RENAME": "platform.organizations.status.manage",
		"ACCOUNT_ORGANIZATION_STATUS_CHANGE": "platform.organizations.status.manage", "ACCOUNT_ORGANIZATION_DELETE": "platform.organizations.delete",
		"ACCOUNT_MEMBER_ADD": "platform.organizations.status.manage", "ACCOUNT_MEMBER_ROLES_REPLACE": "platform.organizations.status.manage",
		"ACCOUNT_MEMBER_REMOVE": "platform.organizations.status.manage", "ACCOUNT_OWNERSHIP_TRANSFER": "platform.organizations.owner.transfer",
		"ACCOUNT_UNIT_CREATE": "platform.organizations.status.manage", "ACCOUNT_UNIT_RENAME": "platform.organizations.status.manage",
		"ACCOUNT_UNIT_DELETE": "platform.organizations.status.manage", "ACCOUNT_REGISTRATION_RETRY": "platform.registrations.repair",
		"ACCOUNT_REGISTRATION_COMPENSATE": "platform.registrations.repair", "ACCOUNT_PROFILE_CHANGE_RETRY": "platform.profile-changes.repair",
		"BILLING_PRICE_VERSION_CREATE": "platform.pricing.manage", "BILLING_PAYMENT_ORDER_SYNC": "platform.billing.orders.sync",
		"BILLING_PAYMENT_ORDER_EVIDENCE_REPAIR": "platform.billing.repairs.request", "BILLING_NORMAL_REFUND": "platform.billing.repairs.request",
		"BILLING_ADJUSTMENT_REQUEST": "platform.billing.repairs.request",
		"BILLING_ADJUSTMENT_REJECT":  "platform.billing.adjustments.approve", "BILLING_USAGE_HOLD_SAFE_RELEASE": "platform.billing.holds.release",
		"BILLING_ORGANIZATION_LIFECYCLE_APPLY":      "internal.billing.organization.lifecycle",
		"BILLING_ORGANIZATION_UNIT_LIFECYCLE_APPLY": "internal.billing.organization-unit.lifecycle",
		"BILLING_RECONCILIATION_RUN":                "platform.usage.reconcile.retry", "TOKEN_ACCOUNT_STATUS_CHANGE": "platform.tokens.manage",
		"TOKEN_ROUTE_UPSERT": "platform.tokens.manage", "TOKEN_ROUTE_DELETE": "platform.tokens.manage",
		"TOKEN_CREDENTIAL_VALIDATE": "platform.tokens.credentials.validate", "TOKEN_USAGE_RECONCILIATION_RETRY": "platform.usage.reconcile.retry",
		"TOKEN_ORGANIZATION_LIFECYCLE_APPLY":      "internal.token.organization.lifecycle",
		"TOKEN_ORGANIZATION_UNIT_LIFECYCLE_APPLY": "internal.token.organization-unit.lifecycle",
		"SUPER_ADMIN_ROLE_HANDOFF":                "platform.roles.assign", "BREAK_GLASS_RECOVERY": "platform.roles.assign",
		"SUPER_ADMIN_BOOTSTRAP": "platform.roles.assign", "APPROVAL_DRAFT_CREATE": "platform.billing.repairs.request",
		"APPROVAL_DRAFT_UPDATE": "platform.billing.repairs.request", "APPROVAL_REQUEST_SUBMIT": "platform.billing.repairs.request",
		"APPROVAL_REQUEST_WITHDRAW": "platform.billing.repairs.request", "APPROVAL_REQUEST_DECIDE": "platform.billing.repairs.approve",
		"APPROVAL_EXECUTION_BEGIN": "internal.approval.execute", "APPROVAL_EXECUTION_COMPLETE": "internal.approval.execute",
		"ACCOUNT_WALLET_PROVISION": "internal.account.wallet.provision", "ACCOUNT_ROLE_BINDING_EXPIRE": "internal.account.role-binding.expire",
		"ACCOUNT_APPROVAL_EXPIRE": "internal.account.approval.expire", "ACCOUNT_PROFILE_CHANGE_RECOVERY": "internal.account.profile-change.recover",
		"ACCOUNT_REGISTRATION_RECOVERY": "internal.account.registration.recover", "ACCOUNT_COMMERCE_CUSTOMER_SYNC": "internal.account.commerce-customer.sync",
		"BILLING_PAYMENT_EXPIRY_RECONCILE": "internal.billing.payment.reconcile", "BILLING_ENTITLEMENT_PACKAGE_EXPIRE": "internal.billing.entitlement.expire",
		"TOKEN_USAGE_RECONCILE": "internal.token.usage.reconcile", "EVENT_OUTBOX_RELAY": "internal.events.relay",
	}
	for commandType, permission := range auditCommands {
		result[commandType] = Policy{
			CommandType: commandType, Mode: AuditOnly, RequesterPermission: permission, RiskClass: "GOVERNANCE_OR_NORMAL_STATE_MACHINE",
			ApprovalCount: 0, SelfApprovalForbidden: false, SnapshotSchemaVersion: 1,
			CriticalResourceFields: []string{"target_id", "request_fingerprint"}, ExpiresAfter: 24 * time.Hour,
			ExecutionWindow: 5 * time.Minute, ExecutorIdentity: "requesting-domain-service",
		}
	}
	return result
}

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

func Executable(commandType string) bool {
	_, ok := executableApprovalCommands[strings.ToUpper(strings.TrimSpace(commandType))]
	return ok
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
		ctx.Header("X-Command-Policy-Version", "1")
		ctx.Next()
	}
}
