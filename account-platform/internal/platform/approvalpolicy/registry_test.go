package approvalpolicy

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestFinancialRepairsRequireOneIndependentApproval(t *testing.T) {
	for _, commandType := range []string{"WALLET_ADJUSTMENT", "LEDGER_REPAIR", "REFUND_EXCEPTION_REPAIR", "USAGE_SETTLEMENT_REPAIR"} {
		policy, err := Lookup(commandType)
		if err != nil {
			t.Fatal(err)
		}
		if policy.Mode != ApprovalRequired || policy.ApprovalCount != 1 || !policy.SelfApprovalForbidden {
			t.Fatalf("unsafe approval policy for %s: %#v", commandType, policy)
		}
	}
}

func TestUnknownCommandFailsClosed(t *testing.T) {
	if _, err := Lookup("UNREGISTERED_FORCE_BALANCE"); !errors.Is(err, ErrPolicyNotRegistered) {
		t.Fatalf("unknown commands must fail closed: %v", err)
	}
}

func TestOnlyImplementedFinancialExecutorsAcceptRequests(t *testing.T) {
	if !Executable("WALLET_ADJUSTMENT") {
		t.Fatal("wallet adjustment executor must be available")
	}
	// LEDGER_REPAIR joins the executable set only so the four Account entry
	// points consult the live Billing activation; its row stays INACTIVE until a
	// separately authorized activation, so no request can advance.
	if !Executable("LEDGER_REPAIR") {
		t.Fatal("ledger repair must consult its live Billing activation")
	}
	// REFUND_EXCEPTION_REPAIR follows the same rule as the other repair
	// commands: membership only opens the live activation consultation.
	if !Executable("REFUND_EXCEPTION_REPAIR") {
		t.Fatal("refund exception repair must consult its live Billing activation")
	}
	if !Executable("CHARGEBACK_FINANCIAL_REPAIR") {
		t.Fatal("chargeback financial repair must consult its live Billing activation")
	}
	if Executable("USAGE_SETTLEMENT_REPAIR") {
		t.Fatal("unimplemented domain repair must remain non-executable")
	}
	if !Executable("PAYMENT_PROVIDER_RESUME") {
		t.Fatal("payment provider resume executor must be available")
	}
	if !Executable("PAYMENT_PRODUCT_RESUME") {
		t.Fatal("payment product resume executor must be available")
	}
}

func TestPaymentControlRecoveryPoliciesRequireIndependentApproval(t *testing.T) {
	for _, commandType := range []string{"PAYMENT_PROVIDER_RESUME", "PAYMENT_PRODUCT_RESUME"} {
		policy, err := Lookup(commandType)
		if err != nil || policy.Mode != ApprovalRequired || policy.ApprovalCount != 1 || !policy.SelfApprovalForbidden || policy.ExecutorIdentity != "billing-command-executor" {
			t.Fatalf("unsafe payment control recovery policy for %s: %#v %v", commandType, policy, err)
		}
	}
	providerPolicy, _ := Lookup("PAYMENT_PROVIDER_RESUME")
	productPolicy, _ := Lookup("PAYMENT_PRODUCT_RESUME")
	if providerPolicy.PolicyVersion != 2 || productPolicy.PolicyVersion != 1 {
		t.Fatalf("payment recovery policy versions must preserve append-only history: provider=%d product=%d", providerPolicy.PolicyVersion, productPolicy.PolicyVersion)
	}
}

func TestNormalRefundAndOrganizationDeleteAreAuditOnly(t *testing.T) {
	for _, commandType := range []string{"BILLING_NORMAL_REFUND", "ACCOUNT_ORGANIZATION_DELETE", "BILLING_ORGANIZATION_UNIT_LIFECYCLE_APPLY", "TOKEN_ORGANIZATION_UNIT_LIFECYCLE_APPLY", "BILLING_ORGANIZATION_MEMBER_LIFECYCLE_APPLY"} {
		policy, err := Lookup(commandType)
		if err != nil || policy.Mode != AuditOnly {
			t.Fatalf("unexpected normal-state policy for %s: %#v %v", commandType, policy, err)
		}
	}
}

func TestOrganizationGovernancePoliciesUseAtomicPermissions(t *testing.T) {
	expected := map[string]string{
		"ACCOUNT_ORGANIZATION_RENAME":  "platform.organizations.profile.manage",
		"ACCOUNT_MEMBER_ADD":           "platform.organizations.members.manage",
		"ACCOUNT_MEMBER_ROLES_REPLACE": "platform.organizations.members.manage",
		"ACCOUNT_MEMBER_REMOVE":        "platform.organizations.members.manage",
		"ACCOUNT_UNIT_CREATE":          "platform.organizations.units.manage",
		"ACCOUNT_UNIT_RENAME":          "platform.organizations.units.manage",
		"ACCOUNT_UNIT_DELETE":          "platform.organizations.units.manage",
	}
	for commandType, permission := range expected {
		policy, err := Lookup(commandType)
		if err != nil {
			t.Fatal(err)
		}
		if policy.PolicyVersion != 2 || policy.Mode != AuditOnly || policy.RequesterPermission != permission {
			t.Fatalf("organization policy must use append-only atomic permission for %s: %#v", commandType, policy)
		}
	}
	statusPolicy, err := Lookup("ACCOUNT_ORGANIZATION_STATUS_CHANGE")
	if err != nil || statusPolicy.PolicyVersion != 1 || statusPolicy.RequesterPermission != "platform.organizations.status.manage" {
		t.Fatalf("status permission must remain isolated on policy version 1: %#v %v", statusPolicy, err)
	}
}

func TestAdminOrganizationUnitMoveUsesInitialAtomicPolicy(t *testing.T) {
	policy, err := Lookup("ACCOUNT_UNIT_MOVE")
	if err != nil || policy.PolicyVersion != 1 || policy.Mode != AuditOnly || policy.RequesterPermission != "platform.organizations.units.manage" {
		t.Fatalf("unexpected organization unit move policy: %#v %v", policy, err)
	}
}

func TestPricingAndMeteringCommandsAreAuditOnlyWithExactPermission(t *testing.T) {
	commands := []string{
		"BILLING_PRICE_DRAFT_CREATE",
		"BILLING_PRICE_DRAFT_UPDATE",
		"BILLING_PRICE_PUBLISH",
		"BILLING_PRICE_TRANSITION",
		"BILLING_METERING_UNIT_CREATE",
		"BILLING_METERING_UNIT_DEACTIVATE",
	}
	for _, commandType := range commands {
		policy, err := Lookup(commandType)
		if err != nil {
			t.Fatalf("pricing policy %s is unavailable: %v", commandType, err)
		}
		if policy.Mode != AuditOnly || policy.RequesterPermission != "platform.pricing.manage" || policy.ApprovalCount != 0 || policy.SelfApprovalForbidden || policy.ExecutorIdentity != "requesting-domain-service" {
			t.Fatalf("unexpected pricing policy for %s: %#v", commandType, policy)
		}
	}
}

func TestTokenProtocolFormulaCommandsAreAuditOnlyWithFrozenFields(t *testing.T) {
	expected := map[string][]string{
		"TOKEN_PROTOCOL_DRAFT_CREATE": {"protocol_code", "expected_protocol_head_version", "request_fingerprint"},
		"TOKEN_PROTOCOL_PUBLISH":      {"protocol_definition_id", "expected_protocol_version", "draft_etag", "request_fingerprint"},
		"TOKEN_FORMULA_DRAFT_CREATE":  {"protocol_definition_id", "expected_protocol_version", "expected_formula_head_version", "request_fingerprint"},
		"TOKEN_FORMULA_PUBLISH":       {"formula_version_id", "expected_formula_version", "draft_etag", "request_fingerprint"},
	}
	for commandType, criticalFields := range expected {
		policy, err := Lookup(commandType)
		if err != nil {
			t.Fatalf("Token protocol/formula policy %s is unavailable: %v", commandType, err)
		}
		if policy.PolicyVersion != 1 || policy.Mode != AuditOnly || policy.RequesterPermission != "platform.pricing.manage" || policy.ApproverPermission != "" || policy.ApprovalCount != 0 || policy.SelfApprovalForbidden || policy.RiskClass != "GOVERNANCE_OR_NORMAL_STATE_MACHINE" || policy.SnapshotSchemaVersion != 1 || !slices.Equal(policy.CriticalResourceFields, criticalFields) || policy.ExecutorIdentity != "requesting-domain-service" {
			t.Fatalf("unexpected Token protocol/formula policy for %s: %#v", commandType, policy)
		}
		if Executable(commandType) {
			t.Fatalf("AUDIT_ONLY command %s must not enter the approval executor allowlist", commandType)
		}
	}
}

func TestNineteenTokenGovernanceCommandsAreRegistered(t *testing.T) {
	commands := []string{
		"TOKEN_PROTOCOL_DRAFT_CREATE", "TOKEN_PROTOCOL_PUBLISH", "TOKEN_FORMULA_DRAFT_CREATE", "TOKEN_FORMULA_PUBLISH",
		"TOKEN_MODEL_DRAFT_SAVE", "TOKEN_MODEL_PUBLISH", "TOKEN_MODEL_LIFECYCLE_CHANGE", "TOKEN_MODEL_CATALOG_SYNC", "TOKEN_MODEL_CATALOG_ROLLBACK",
		"TOKEN_ACCOUNT_CREATE", "TOKEN_ACCOUNT_STATUS_CHANGE", "TOKEN_ROUTE_UPSERT", "TOKEN_ROUTE_DELETE",
		"TOKEN_CREDENTIAL_ENROLL", "TOKEN_CREDENTIAL_ROTATE", "TOKEN_CREDENTIAL_REVOKE", "TOKEN_CREDENTIAL_VALIDATE",
		"TOKEN_ROUTE_PROVISIONING_REPAIR", "TOKEN_USAGE_RECONCILIATION_RETRY",
	}
	for _, commandType := range commands {
		if _, err := Lookup(commandType); err != nil {
			t.Fatalf("Token governance command %s is not registered: %v", commandType, err)
		}
	}
}

func TestTokenApprovalPoliciesAndExecutableAllowlistAreFrozen(t *testing.T) {
	expected := map[string]Policy{
		"TOKEN_MODEL_LIFECYCLE_CHANGE": {
			CommandType: "TOKEN_MODEL_LIFECYCLE_CHANGE", PolicyVersion: 2, Mode: ApprovalRequired,
			RequesterPermission: "platform.models.lifecycle.request", ApproverPermission: "platform.models.lifecycle.approve",
			RiskClass: "GOVERNANCE_OR_NORMAL_STATE_MACHINE", ApprovalCount: 1, SelfApprovalForbidden: true, SnapshotSchemaVersion: 2,
			CriticalResourceFields: []string{"model_id", "catalog_version", "current_lifecycle", "target_lifecycle", "lifecycle_plan_version", "effective_at", "existing_session_cutoff_at", "replacement_model_id", "billing_price_version", "route_set_fingerprint", "provisioning_set_fingerprint", "inflight_request_set_fingerprint", "policy_version"},
			ExpiresAfter:           24 * time.Hour, ExecutionWindow: 5 * time.Minute, ExecutorIdentity: "token-command-executor",
		},
		"TOKEN_MODEL_CATALOG_ROLLBACK": {
			CommandType: "TOKEN_MODEL_CATALOG_ROLLBACK", PolicyVersion: 2, Mode: ApprovalRequired,
			RequesterPermission: "platform.models.catalog.rollback.request", ApproverPermission: "platform.models.catalog.rollback.approve",
			RiskClass: "GOVERNANCE_OR_NORMAL_STATE_MACHINE", ApprovalCount: 1, SelfApprovalForbidden: true, SnapshotSchemaVersion: 2,
			CriticalResourceFields: []string{"current_catalog_version", "source_catalog_version", "catalog_diff_fingerprint", "billing_price_version", "route_set_fingerprint", "provisioning_set_fingerprint", "inflight_request_set_fingerprint", "policy_version"},
			ExpiresAfter:           24 * time.Hour, ExecutionWindow: 5 * time.Minute, ExecutorIdentity: "token-command-executor",
		},
		"TOKEN_CREDENTIAL_REVOKE": {
			CommandType: "TOKEN_CREDENTIAL_REVOKE", PolicyVersion: 1, Mode: ApprovalRequired,
			RequesterPermission: "platform.tokens.credentials.revoke.request", ApproverPermission: "platform.tokens.credentials.revoke.approve",
			RiskClass: "GOVERNANCE_OR_NORMAL_STATE_MACHINE", ApprovalCount: 1, SelfApprovalForbidden: true, SnapshotSchemaVersion: 1,
			CriticalResourceFields: []string{"token_account_id", "token_account_version", "credential_id", "credential_version", "credential_status", "credential_fingerprint", "active_route_set_fingerprint", "provisioning_lease_set_fingerprint", "inflight_request_set_fingerprint", "replacement_credential_version", "policy_version"},
			ExpiresAfter:           24 * time.Hour, ExecutionWindow: 5 * time.Minute, ExecutorIdentity: "token-command-executor",
		},
	}
	for commandType, want := range expected {
		got, err := Lookup(commandType)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("Token approval policy mismatch for %s:\n got %#v\nwant %#v\nerr %v", commandType, got, want, err)
		}
	}
	for _, existing := range []string{"WALLET_ADJUSTMENT", "PAYMENT_PROVIDER_RESUME", "PAYMENT_PRODUCT_RESUME"} {
		if !Executable(existing) {
			t.Fatalf("existing executable command %s was removed", existing)
		}
	}
	for commandType := range tokenCommandSpecsForTest() {
		_, shouldExecute := expected[commandType]
		if Executable(commandType) != shouldExecute {
			t.Fatalf("unexpected Token executable allowlist state for %s", commandType)
		}
	}
}

func tokenCommandSpecsForTest() map[string]struct{} {
	result := map[string]struct{}{}
	for _, policy := range All() {
		if strings.HasPrefix(policy.CommandType, "TOKEN_") && policy.CommandType != "TOKEN_ORGANIZATION_MODEL_POLICY_UPDATE" && !strings.HasSuffix(policy.CommandType, "_LIFECYCLE_APPLY") && policy.CommandType != "TOKEN_USAGE_RECONCILE" {
			result[policy.CommandType] = struct{}{}
		}
	}
	return result
}
