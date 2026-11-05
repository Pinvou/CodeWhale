package approvalpolicy

import (
	"slices"
	"testing"
	"time"
)

func TestF17AuditOnlyPolicies(t *testing.T) {
	for _, c := range []struct{ name, permission string }{{"ACCOUNT_AUDIT_EXPORT_CREATE", "platform.audit.export"}, {"ACCOUNT_AUDIT_EXPORT_DOWNLOAD_TICKET", "platform.audit.export"}} {
		p, err := Lookup(c.name)
		if err != nil || p.Mode != AuditOnly || p.PolicyVersion != 1 || p.RequesterPermission != c.permission || p.ApprovalCount != 0 || Executable(c.name) || TokenExecutableApproval(c.name) {
			t.Fatalf("audit policy differs: %s", c.name)
		}
		if p.SnapshotSchemaVersion != 1 || p.ApproverPermission != "" || p.SelfApprovalForbidden ||
			p.RiskClass != "GOVERNANCE_OR_NORMAL_STATE_MACHINE" || p.ExecutorIdentity != "requesting-domain-service" ||
			p.ExpiresAfter != 24*time.Hour || p.ExecutionWindow != 5*time.Minute ||
			!slices.Equal(p.CriticalResourceFields, []string{"target_id", "request_fingerprint"}) {
			t.Fatal("audit-only policy differs from original migration", c.name, p)
		}
	}
}
