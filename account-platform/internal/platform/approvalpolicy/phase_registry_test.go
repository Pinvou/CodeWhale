package approvalpolicy

import "testing"

func TestInstalledBillingPolicySetAndFutureDeny(t *testing.T) {
	billing := map[string]int64{"PAYMENT_FINANCIAL_REPAIR": 1, "ENTITLEMENT_COMPONENT_ADJUSTMENT": 2, "LEDGER_REPAIR": 1, "RECEIVABLE_WRITE_OFF": 1, "WALLET_ADJUSTMENT": 1, "PAYMENT_PROVIDER_RESUME": 2, "PAYMENT_PRODUCT_RESUME": 1}
	token := map[string]int64{
		"TOKEN_PROTOCOL_DRAFT_CREATE": 1, "TOKEN_PROTOCOL_PUBLISH": 1, "TOKEN_FORMULA_DRAFT_CREATE": 1, "TOKEN_FORMULA_PUBLISH": 1,
		"TOKEN_MODEL_DRAFT_SAVE": 2, "TOKEN_MODEL_PUBLISH": 2, "TOKEN_MODEL_LIFECYCLE_CHANGE": 2, "TOKEN_MODEL_CATALOG_SYNC": 2, "TOKEN_MODEL_CATALOG_ROLLBACK": 2,
		"TOKEN_ACCOUNT_CREATE": 3, "TOKEN_ACCOUNT_STATUS_CHANGE": 2, "TOKEN_ROUTE_UPSERT": 3, "TOKEN_ROUTE_DELETE": 2,
		"TOKEN_USAGE_RECONCILIATION_RETRY": 2, "TOKEN_CREDENTIAL_ENROLL": 1, "TOKEN_CREDENTIAL_ROTATE": 1, "TOKEN_CREDENTIAL_REVOKE": 1, "TOKEN_CREDENTIAL_VALIDATE": 2, "TOKEN_ROUTE_PROVISIONING_REPAIR": 2,
	}
	executableToken := map[string]bool{"TOKEN_MODEL_LIFECYCLE_CHANGE": true, "TOKEN_MODEL_CATALOG_ROLLBACK": true, "TOKEN_CREDENTIAL_REVOKE": true}
	for name, version := range billing {
		p, err := Lookup(name)
		if err != nil || p.PolicyVersion != version || !Executable(name) || p.Mode != ApprovalRequired || !p.SelfApprovalForbidden || p.ExecutorIdentity != "billing-command-executor" || TokenExecutableApproval(name) {
			t.Fatalf("bad installed Billing policy: %s", name)
		}
	}
	for name, version := range token {
		p, err := Lookup(name)
		executable := executableToken[name]
		if err != nil || p.PolicyVersion != version || Executable(name) != executable || TokenExecutableApproval(name) != executable {
			t.Fatalf("bad installed Token policy: %s", name)
		}
		if executable {
			if p.Mode != ApprovalRequired || !p.SelfApprovalForbidden || p.ApprovalCount != 1 || p.ExecutorIdentity != "token-command-executor" {
				t.Fatalf("Token approval authority differs: %s", name)
			}
		} else if p.Mode != AuditOnly || p.ApprovalCount != 0 || p.SelfApprovalForbidden || p.ExecutorIdentity != "requesting-domain-service" {
			t.Fatalf("Token audit-only policy became executable: %s", name)
		}
	}
	for _, name := range []string{"MODEL_CATALOG_PUBLISH", "REFUND_FORCE_APPLY", "RECONCILIATION_FINANCIAL_REPAIR", "SUPER_ADMIN_ROLE_HANDOFF"} {
		if _, err := Lookup(name); err == nil || Executable(name) || TokenExecutableApproval(name) {
			t.Fatalf("future command advertised: %s", name)
		}
	}
	for _, name := range []string{"TOKEN_ORGANIZATION_LIFECYCLE_APPLY", "TOKEN_ORGANIZATION_UNIT_LIFECYCLE_APPLY", "ACCOUNT_CLOSURE_RETRY", "ACCOUNT_MEMBER_UNITS_REPLACE", "BILLING_REGISTRATION_BONUS_POLICY_UPDATE", "TOKEN_ORGANIZATION_MODEL_POLICY_UPDATE", "ACCOUNT_ORGANIZATION_CREATE", "ACCOUNT_USER_STATUS_CHANGE", "ACCOUNT_REGISTRATION_RETRY", "ACCOUNT_REGISTRATION_COMPENSATE", "ACCOUNT_PROFILE_CHANGE_RETRY", "ACCOUNT_SMS_CONFIGURATION_UPDATE", "APPROVAL_DRAFT_CREATE", "APPROVAL_DRAFT_UPDATE", "APPROVAL_REQUEST_SUBMIT", "APPROVAL_REQUEST_WITHDRAW", "APPROVAL_REQUEST_DECIDE", "APPROVAL_EXECUTION_BEGIN", "APPROVAL_EXECUTION_COMPLETE"} {
		p, err := Lookup(name)
		if err != nil || p.Mode != AuditOnly || Executable(name) {
			t.Fatalf("workflow classifier lost: %s", name)
		}
	}
	policies, err := governancePolicies()
	if err != nil || len(policies) != len(billing)+len(token) {
		t.Fatal("database manifest set differs")
	}
	seen := make(map[string]bool)
	for _, p := range policies {
		version, ok := billing[p.CommandType]
		if !ok {
			version, ok = token[p.CommandType]
		}
		if !ok || p.PolicyVersion != version || seen[p.CommandType] {
			t.Fatalf("database manifest command differs: %s", p.CommandType)
		}
		seen[p.CommandType] = true
	}
	digest, err := PolicySetDigest()
	if err != nil || len(digest) != 64 {
		t.Fatal("manifest digest unavailable")
	}
}
