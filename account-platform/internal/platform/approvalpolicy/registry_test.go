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
}

func TestNormalRefundAndOrganizationDeleteAreAuditOnly(t *testing.T) {
	for _, commandType := range []string{"BILLING_NORMAL_REFUND", "ACCOUNT_ORGANIZATION_DELETE", "BILLING_ORGANIZATION_UNIT_LIFECYCLE_APPLY", "TOKEN_ORGANIZATION_UNIT_LIFECYCLE_APPLY", "BILLING_ORGANIZATION_MEMBER_LIFECYCLE_APPLY"} {
		policy, err := Lookup(commandType)
		if err != nil || policy.Mode != AuditOnly {
			t.Fatalf("unexpected normal-state policy for %s: %#v %v", commandType, policy, err)
		}
	}
}

func TestProtocolAndFormulaGovernanceUseExistingPricingPermission(t *testing.T) {
	for _, commandType := range []string{"TOKEN_PROTOCOL_DRAFT_CREATE", "TOKEN_PROTOCOL_PUBLISH", "TOKEN_FORMULA_DRAFT_CREATE", "TOKEN_FORMULA_PUBLISH"} {
		t.Run(commandType, func(t *testing.T) {
			policy, err := Lookup(commandType)
			if err != nil || policy.Mode != AuditOnly || policy.RequesterPermission != "platform.pricing.manage" || policy.ApprovalCount != 0 || policy.ExecutorIdentity != "requesting-domain-service" {
				t.Fatalf("governance route policy must preserve the existing pricing permission and normal-state audit: %#v %v", policy, err)
			}
		})
	}
}
