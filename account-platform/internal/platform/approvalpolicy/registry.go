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
		"APPROVAL_DRAFT_CREATE":                         "platform.billing.repairs.request",
		"APPROVAL_DRAFT_UPDATE":                         "platform.billing.repairs.request",
		"APPROVAL_REQUEST_SUBMIT":                       "platform.billing.repairs.request",
		"APPROVAL_REQUEST_WITHDRAW":                     "platform.billing.repairs.request",
		"APPROVAL_REQUEST_DECIDE":                       "platform.billing.repairs.approve",
		"APPROVAL_EXECUTION_BEGIN":                      "internal.approval.execute",
		"APPROVAL_EXECUTION_COMPLETE":                   "internal.approval.execute",
		"BILLING_ADJUSTMENT_REJECT":                     "platform.billing.adjustments.approve",
		"BILLING_ADJUSTMENT_REQUEST":                    "platform.billing.repairs.request",
		"BILLING_CHARGEBACK_RECORD":                     "internal.billing.payment.reconcile",
		"BILLING_FORCED_REFUND_RECORD":                  "internal.billing.payment.reconcile",
		"BILLING_LATE_PAYMENT_ARCHIVE":                  "platform.billing.orders.sync",
		"BILLING_LATE_PAYMENT_CLAIM":                    "platform.billing.orders.sync",
		"BILLING_LATE_PAYMENT_QUERY":                    "platform.billing.orders.sync",
		"BILLING_LATE_PAYMENT_SCHEDULE":                 "platform.billing.orders.sync",
		"BILLING_NORMAL_REFUND":                         "platform.billing.repairs.request",
		"BILLING_PAYMENT_ANOMALY_CLAIM":                 "platform.billing.orders.sync",
		"BILLING_PAYMENT_ANOMALY_RESOLVE":               "platform.billing.repairs.request",
		"BILLING_PAYMENT_ORDER_EVIDENCE_REPAIR":         "platform.billing.repairs.request",
		"BILLING_PAYMENT_ORDER_SYNC":                    "platform.billing.orders.sync",
		"BILLING_PROVIDER_BLIND_ESCALATE":               "platform.billing.orders.sync",
		"BILLING_RECHARGE_PRODUCT_ADMISSION_REVOKE":     "platform.products.publish",
		"BILLING_RECHARGE_PRODUCT_ADMISSION_VERIFY":     "platform.products.publish",
		"BILLING_RECHARGE_PRODUCT_CREATE":               "platform.products.create",
		"BILLING_RECHARGE_PRODUCT_DELETE":               "platform.products.delete",
		"BILLING_RECHARGE_PRODUCT_DISPLAY_NAMES_UPDATE": "platform.products.publish",
		"BILLING_RECHARGE_PRODUCT_DRAFT_UPDATE":         "platform.products.create",
		"BILLING_RECHARGE_PRODUCT_PUBLISH":              "platform.products.publish",
		"BILLING_RECHARGE_PRODUCT_UNPUBLISH":            "platform.products.unpublish",
		"BILLING_RECONCILIATION_RUN":                    "platform.billing.reconciliation.run",
		"BILLING_PROVIDER_BILL_IMPORT":                  "platform.billing.reconciliation.run",
		"BILLING_RECONCILIATION_CASE_UPDATE":            "platform.billing.reconciliation.manage",
		"BILLING_DAILY_RECONCILIATION":                  "internal.billing.payment.reconcile",
		"BILLING_PAYMENT_EXPIRY_RECONCILE":              "internal.billing.payment.reconcile",
		"BILLING_ENTITLEMENT_PACKAGE_EXPIRE":            "internal.billing.entitlement.expire",
		"BILLING_PRICE_DRAFT_CREATE":                    "platform.pricing.manage",
		"BILLING_PRICE_DRAFT_UPDATE":                    "platform.pricing.manage",
		"BILLING_PRICE_PUBLISH":                         "platform.pricing.manage",
		"BILLING_PRICE_TRANSITION":                      "platform.pricing.manage",
		"BILLING_METERING_UNIT_CREATE":                  "platform.pricing.manage",
		"BILLING_METERING_UNIT_DEACTIVATE":              "platform.pricing.manage",
		"BILLING_MANUAL_REVIEW_SLA_ESCALATE":            "internal.billing.manual-review.escalate",
		"EVENT_OUTBOX_RELAY":                            "internal.events.relay",
		"ACCOUNT_USER_STATUS_CHANGE":                    "platform.users.status.manage",
		"ACCOUNT_REGISTRATION_RETRY":                    "platform.registrations.repair",
		"ACCOUNT_REGISTRATION_COMPENSATE":               "platform.registrations.repair",
		"ACCOUNT_PROFILE_CHANGE_RETRY":                  "platform.profile-changes.repair",
		"ACCOUNT_SMS_CONFIGURATION_UPDATE":              "platform.sms-config.manage",
	} {
		result[commandType] = Policy{CommandType: commandType, PolicyVersion: Version, Mode: AuditOnly,
			RequesterPermission: permission, RiskClass: "GOVERNANCE_OR_NORMAL_STATE_MACHINE",
			ApprovalCount: 0, SelfApprovalForbidden: false, SnapshotSchemaVersion: 1,
			CriticalResourceFields: []string{"target_id", "request_fingerprint"},
			ExpiresAfter:           24 * time.Hour, ExecutionWindow: 5 * time.Minute, ExecutorIdentity: "requesting-domain-service"}
	}
	for _, command := range []string{"BILLING_RECONCILIATION_RUN", "BILLING_PROVIDER_BILL_IMPORT", "BILLING_RECONCILIATION_CASE_UPDATE"} {
		policy := result[command]
		policy.PolicyVersion = 2
		result[command] = policy
	}
	result["WALLET_ADJUSTMENT"] = Policy{
		CommandType: "WALLET_ADJUSTMENT", PolicyVersion: 1, Mode: ApprovalRequired,
		RequesterPermission: "platform.billing.repairs.request", ApproverPermission: "platform.billing.repairs.approve",
		RiskClass: "FINANCIAL_FACT_CHANGE", ApprovalCount: 1, SelfApprovalForbidden: true, SnapshotSchemaVersion: 1,
		CriticalResourceFields: []string{"target_id", "resource_version", "financial_snapshot", "policy_version"},
		ExpiresAfter:           24 * time.Hour, ExecutionWindow: 5 * time.Minute, ExecutorIdentity: "billing-command-executor",
	}
	result["PAYMENT_PROVIDER_RESUME"] = Policy{
		CommandType: "PAYMENT_PROVIDER_RESUME", PolicyVersion: 2, Mode: ApprovalRequired,
		RequesterPermission: "platform.billing.payment-controls.resume.request", ApproverPermission: "platform.billing.payment-controls.resume.approve",
		RiskClass: "FINANCIAL_FACT_CHANGE", ApprovalCount: 1, SelfApprovalForbidden: true,
		SnapshotSchemaVersion: 1, CriticalResourceFields: []string{"provider_id", "provider_version", "provider_control_version", "probe_order_id", "policy_version", "observation_high_watermark", "evaluation_high_watermark", "active_mapping_set_fingerprint"},
		ExpiresAfter: 24 * time.Hour, ExecutionWindow: 5 * time.Minute, ExecutorIdentity: "billing-command-executor",
	}
	result["PAYMENT_PRODUCT_RESUME"] = Policy{
		CommandType: "PAYMENT_PRODUCT_RESUME", PolicyVersion: Version, Mode: ApprovalRequired,
		RequesterPermission: "platform.billing.payment-controls.resume.request", ApproverPermission: "platform.billing.payment-controls.resume.approve",
		RiskClass: "FINANCIAL_FACT_CHANGE", ApprovalCount: 1, SelfApprovalForbidden: true,
		SnapshotSchemaVersion: 1, CriticalResourceFields: []string{"provider_id", "provider_version", "product_id", "provider_control_version", "product_control_version", "catalog_item_id", "catalog_version", "admission_version", "mapping_id", "mapping_version", "probe_order_id", "policy_version", "observation_high_watermark", "evaluation_high_watermark"},
		ExpiresAfter: 24 * time.Hour, ExecutionWindow: 5 * time.Minute, ExecutorIdentity: "billing-command-executor",
	}
	addTokenGovernancePolicies(result)
	return result
}

// Executable is the installed, closed approval command set.
func Executable(command string) bool {
	switch strings.ToUpper(strings.TrimSpace(command)) {
	case "WALLET_ADJUSTMENT", "PAYMENT_PROVIDER_RESUME", "PAYMENT_PRODUCT_RESUME", "TOKEN_MODEL_LIFECYCLE_CHANGE", "TOKEN_MODEL_CATALOG_ROLLBACK", "TOKEN_CREDENTIAL_REVOKE":
		return true
	}
	return false
}

func addTokenGovernancePolicies(result map[string]Policy) {
	audit := func(commandType string, version int64, permission string, snapshotVersion int, fields ...string) {
		result[commandType] = Policy{
			CommandType: commandType, PolicyVersion: version, Mode: AuditOnly,
			RequesterPermission: permission, RiskClass: "GOVERNANCE_OR_NORMAL_STATE_MACHINE",
			ApprovalCount: 0, SelfApprovalForbidden: false, SnapshotSchemaVersion: snapshotVersion,
			CriticalResourceFields: fields, ExpiresAfter: 24 * time.Hour,
			ExecutionWindow: 5 * time.Minute, ExecutorIdentity: "requesting-domain-service",
		}
	}
	// The installed eighteen management policies retain the immutable Account
	// migration contracts. Usage reconciliation is installed with F05 execution.
	audit("TOKEN_PROTOCOL_DRAFT_CREATE", 1, "platform.pricing.manage", 1, "protocol_code", "expected_protocol_head_version", "request_fingerprint")
	audit("TOKEN_PROTOCOL_PUBLISH", 1, "platform.pricing.manage", 1, "protocol_definition_id", "expected_protocol_version", "draft_etag", "request_fingerprint")
	audit("TOKEN_FORMULA_DRAFT_CREATE", 1, "platform.pricing.manage", 1, "protocol_definition_id", "expected_protocol_version", "expected_formula_head_version", "request_fingerprint")
	audit("TOKEN_FORMULA_PUBLISH", 1, "platform.pricing.manage", 1, "formula_version_id", "expected_formula_version", "draft_etag", "request_fingerprint")

	audit("TOKEN_MODEL_DRAFT_SAVE", 2, "platform.models.manage", 2, "model_id", "expected_catalog_version", "expected_draft_version", "precheck_fingerprint", "request_fingerprint")
	audit("TOKEN_MODEL_PUBLISH", 2, "platform.models.manage", 2, "model_id", "expected_catalog_version", "expected_draft_version", "precheck_fingerprint", "request_fingerprint")
	result["TOKEN_MODEL_LIFECYCLE_CHANGE"] = Policy{
		CommandType: "TOKEN_MODEL_LIFECYCLE_CHANGE", PolicyVersion: 2, Mode: ApprovalRequired,
		RequesterPermission: "platform.models.lifecycle.request", ApproverPermission: "platform.models.lifecycle.approve",
		RiskClass: "GOVERNANCE_OR_NORMAL_STATE_MACHINE", ApprovalCount: 1, SelfApprovalForbidden: true,
		SnapshotSchemaVersion: 2, CriticalResourceFields: []string{"model_id", "catalog_version", "current_lifecycle", "target_lifecycle", "lifecycle_plan_version", "effective_at", "existing_session_cutoff_at", "replacement_model_id", "billing_price_version", "route_set_fingerprint", "provisioning_set_fingerprint", "inflight_request_set_fingerprint", "policy_version"},
		ExpiresAfter: 24 * time.Hour, ExecutionWindow: 5 * time.Minute, ExecutorIdentity: "token-command-executor",
	}
	audit("TOKEN_MODEL_CATALOG_SYNC", 2, "platform.models.sync", 2, "expected_catalog_version", "adapter_observed_version", "catalog_diff_fingerprint", "request_fingerprint")
	result["TOKEN_MODEL_CATALOG_ROLLBACK"] = Policy{
		CommandType: "TOKEN_MODEL_CATALOG_ROLLBACK", PolicyVersion: 2, Mode: ApprovalRequired,
		RequesterPermission: "platform.models.catalog.rollback.request", ApproverPermission: "platform.models.catalog.rollback.approve",
		RiskClass: "GOVERNANCE_OR_NORMAL_STATE_MACHINE", ApprovalCount: 1, SelfApprovalForbidden: true,
		SnapshotSchemaVersion: 2, CriticalResourceFields: []string{"current_catalog_version", "source_catalog_version", "catalog_diff_fingerprint", "billing_price_version", "route_set_fingerprint", "provisioning_set_fingerprint", "inflight_request_set_fingerprint", "policy_version"},
		ExpiresAfter: 24 * time.Hour, ExecutionWindow: 5 * time.Minute, ExecutorIdentity: "token-command-executor",
	}
	audit("TOKEN_ACCOUNT_CREATE", 3, "platform.tokens.manage", 3, "owner_type", "owner_id", "owner_authorization_version", "owner_version", "owner_identity_digest", "owner_account_count", "owner_account_cap", "owner_account_cap_policy_version", "creation_version", "precheck_fingerprint", "request_fingerprint")
	audit("TOKEN_ACCOUNT_STATUS_CHANGE", 2, "platform.tokens.manage", 2, "token_account_id", "expected_account_version", "target_status", "precheck_fingerprint", "request_fingerprint")
	audit("TOKEN_ROUTE_UPSERT", 3, "platform.tokens.manage", 3, "token_account_id", "expected_account_version", "expected_route_version", "model_id", "protocol_version_id", "precheck_fingerprint", "request_fingerprint")
	audit("TOKEN_ROUTE_DELETE", 2, "platform.tokens.manage", 2, "token_account_id", "route_id", "expected_account_version", "expected_route_version", "precheck_fingerprint", "request_fingerprint")
	audit("TOKEN_CREDENTIAL_ENROLL", 1, "platform.tokens.manage", 1, "token_account_id", "expected_account_version", "credential_slot_version", "launch_fingerprint")
	audit("TOKEN_CREDENTIAL_ROTATE", 1, "platform.tokens.manage", 1, "token_account_id", "expected_account_version", "active_credential_version", "launch_fingerprint")
	result["TOKEN_CREDENTIAL_REVOKE"] = Policy{
		CommandType: "TOKEN_CREDENTIAL_REVOKE", PolicyVersion: 1, Mode: ApprovalRequired,
		RequesterPermission: "platform.tokens.credentials.revoke.request", ApproverPermission: "platform.tokens.credentials.revoke.approve",
		RiskClass: "GOVERNANCE_OR_NORMAL_STATE_MACHINE", ApprovalCount: 1, SelfApprovalForbidden: true,
		SnapshotSchemaVersion: 1, CriticalResourceFields: []string{"token_account_id", "token_account_version", "credential_id", "credential_version", "credential_status", "credential_fingerprint", "active_route_set_fingerprint", "provisioning_lease_set_fingerprint", "inflight_request_set_fingerprint", "replacement_credential_version", "policy_version"},
		ExpiresAfter: 24 * time.Hour, ExecutionWindow: 5 * time.Minute, ExecutorIdentity: "token-command-executor",
	}
	audit("TOKEN_CREDENTIAL_VALIDATE", 2, "platform.tokens.credentials.validate", 2, "token_account_id", "expected_account_version", "credential_id", "credential_version", "precheck_fingerprint", "request_fingerprint")
	audit("TOKEN_ROUTE_PROVISIONING_REPAIR", 2, "platform.tokens.manage", 2, "provisioning_id", "expected_provisioning_version", "target_generation", "precheck_fingerprint", "request_fingerprint")
}

// TokenExecutableApproval is the delivered, finite Token approval family.
// Account additionally requires the persistent current governance activation.
func TokenExecutableApproval(commandType string) bool {
	switch strings.ToUpper(strings.TrimSpace(commandType)) {
	case "TOKEN_MODEL_LIFECYCLE_CHANGE", "TOKEN_MODEL_CATALOG_ROLLBACK", "TOKEN_CREDENTIAL_REVOKE":
		return true
	}
	return false
}
