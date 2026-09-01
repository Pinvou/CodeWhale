package approvalpolicy

import (
	"errors"
	"testing"
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
	if Executable("LEDGER_REPAIR") {
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
