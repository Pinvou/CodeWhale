package approvalpolicy

import (
	"testing"
	"time"
)

func TestPlatformRoleBindingOriginalAuditOnlyPolicies(t *testing.T) {
	for _, name := range []string{"ACCOUNT_ROLE_BINDING_GRANT", "ACCOUNT_ROLE_BINDING_REVOKE"} {
		p, err := Lookup(name)
		if err != nil || p.Mode != AuditOnly || p.PolicyVersion != 1 || p.RequesterPermission != "platform.roles.assign" || p.ApproverPermission != "" || p.ApprovalCount != 0 || p.SelfApprovalForbidden || p.ExecutorIdentity != "requesting-domain-service" || p.ExpiresAfter != 24*time.Hour || p.ExecutionWindow != 5*time.Minute || Executable(name) || TokenExecutableApproval(name) {
			t.Fatal("original role policy changed", name, p, err)
		}
	}
	for _, name := range []string{"SUPER_ADMIN_ROLE_HANDOFF", "BREAK_GLASS_RECOVERY", "SUPER_ADMIN_BOOTSTRAP"} {
		if _, err := Lookup(name); err == nil {
			t.Fatal("privileged recovery was exposed", name)
		}
	}
}
