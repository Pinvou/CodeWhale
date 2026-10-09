package approvalpolicy

import "testing"

func TestF17AuditOnlyPolicies(t *testing.T) {
	for _, c := range []struct{ name, permission string }{{"ACCOUNT_AUDIT_EXPORT_CREATE", "platform.audit.export"}, {"ACCOUNT_AUDIT_EXPORT_DOWNLOAD_TICKET", "platform.audit.export"}} {
		p, err := Lookup(c.name)
		if err != nil || p.Mode != AuditOnly || p.PolicyVersion != 1 || p.RequesterPermission != c.permission || p.ApprovalCount != 0 || Executable(c.name) || TokenExecutableApproval(c.name) {
			t.Fatalf("audit policy differs: %s", c.name)
		}
	}
}
