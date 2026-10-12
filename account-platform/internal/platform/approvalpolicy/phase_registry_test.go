package approvalpolicy

import "testing"

func TestInstalledBillingPolicySetAndFutureDeny(t *testing.T) {
	want := map[string]int64{"WALLET_ADJUSTMENT": 1, "PAYMENT_PROVIDER_RESUME": 2, "PAYMENT_PRODUCT_RESUME": 1}
	for name, version := range want {
		p, err := Lookup(name)
		if err != nil || p.PolicyVersion != version || !Executable(name) || p.Mode != ApprovalRequired || !p.SelfApprovalForbidden || p.ExecutorIdentity != "billing-command-executor" {
			t.Fatalf("bad installed policy: %s", name)
		}
	}
	for _, name := range []string{"MODEL_CATALOG_PUBLISH", "TOKEN_CREDENTIAL_REVOKE", "REFUND_FORCE_APPLY", "RECEIVABLE_WRITE_OFF", "ENTITLEMENT_COMPONENT_ADJUSTMENT", "SUPER_ADMIN_ROLE_HANDOFF"} {
		if _, err := Lookup(name); err == nil || Executable(name) {
			t.Fatalf("future command advertised: %s", name)
		}
	}
	for _, name := range []string{"ACCOUNT_USER_STATUS_CHANGE", "ACCOUNT_REGISTRATION_RETRY", "ACCOUNT_REGISTRATION_COMPENSATE", "ACCOUNT_PROFILE_CHANGE_RETRY", "ACCOUNT_SMS_CONFIGURATION_UPDATE", "APPROVAL_DRAFT_CREATE", "APPROVAL_DRAFT_UPDATE", "APPROVAL_REQUEST_SUBMIT", "APPROVAL_REQUEST_WITHDRAW", "APPROVAL_REQUEST_DECIDE", "APPROVAL_EXECUTION_BEGIN", "APPROVAL_EXECUTION_COMPLETE"} {
		p, err := Lookup(name)
		if err != nil || p.Mode != AuditOnly || Executable(name) {
			t.Fatalf("workflow classifier lost: %s", name)
		}
	}
	policies, err := governancePolicies()
	if err != nil || len(policies) != 3 {
		t.Fatal("database manifest expanded")
	}
	digest, err := PolicySetDigest()
	if err != nil || len(digest) != 64 {
		t.Fatal("manifest digest unavailable")
	}
}
