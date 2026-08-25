package account

import "testing"

func TestCanonicalPermissionCodeMigratesOrganizationAliases(t *testing.T) {
	tests := map[string]string{
		"model.invoke":                         "org.model.invoke",
		"organization.grant.reclaim_closed":    "org.grants.reclaim_closed",
		" org.model.invoke ":                   "org.model.invoke",
		"platform.tokens.credentials.validate": "platform.tokens.credentials.validate",
		"personal.session.login":               "personal.session.login",
	}
	for input, expected := range tests {
		if actual := canonicalPermissionCode(input); actual != expected {
			t.Fatalf("canonicalPermissionCode(%q)=%q, want %q", input, actual, expected)
		}
	}
}

func TestPermissionLifecycleIsFailClosed(t *testing.T) {
	allowed := []struct {
		status     string
		permission string
	}{
		{"ACTIVE", "personal.recharge.create"},
		{"ACTIVE", "platform.users.read"},
		{"PROVISIONING", PermissionPersonalSessionLogin},
		{"PROVISIONING", PermissionPersonalSecurityManage},
		{"REPAIR_REQUIRED", PermissionPersonalProfileUpdate},
		{"CLOSING", PermissionPersonalAccountClosureRead},
	}
	for _, item := range allowed {
		if !permissionAllowedForUserStatus(item.status, item.permission) {
			t.Fatalf("expected %s to allow %s", item.status, item.permission)
		}
	}

	denied := []struct {
		status     string
		permission string
	}{
		{"PROVISIONING", "personal.recharge.create"},
		{"REPAIR_REQUIRED", "personal.model.invoke"},
		{"CLOSING", PermissionPersonalSessionLogin},
		{"DISABLED", PermissionPersonalSessionLogin},
		{"DELETED", PermissionPersonalAccountClosureRead},
		{"BOOTSTRAP_PENDING", PermissionPersonalSecurityManage},
		{"UNKNOWN", "personal.profile.read"},
	}
	for _, item := range denied {
		if permissionAllowedForUserStatus(item.status, item.permission) {
			t.Fatalf("expected %s to deny %s", item.status, item.permission)
		}
	}
}

func TestPersonalRoleEligibilityIsLifecycleOwned(t *testing.T) {
	for _, status := range []string{"BOOTSTRAP_PENDING", "PROVISIONING", "ACTIVE", "REPAIR_REQUIRED"} {
		if !personalRoleEligibleStatus(status) {
			t.Fatalf("expected %s to retain the personal role", status)
		}
	}
	for _, status := range []string{"CLOSING", "DISABLED", "DELETED", "ANONYMIZED", "UNKNOWN"} {
		if personalRoleEligibleStatus(status) {
			t.Fatalf("expected %s not to create a personal role", status)
		}
	}
}
