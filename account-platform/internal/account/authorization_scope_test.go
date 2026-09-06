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

func TestCompositePermissionBranchesUseBooleanIdentity(t *testing.T) {
	tests := []struct {
		name        string
		any         []string
		all         []string
		granted     map[string]struct{}
		wantAllowed bool
	}{
		{name: "any only granted", any: []string{"a"}, granted: map[string]struct{}{"a": {}}, wantAllowed: true},
		{name: "any only denied", any: []string{"a"}, granted: map[string]struct{}{}, wantAllowed: false},
		{name: "all only granted", all: []string{"a", "b"}, granted: map[string]struct{}{"a": {}, "b": {}}, wantAllowed: true},
		{name: "all only partially granted", all: []string{"a", "b"}, granted: map[string]struct{}{"a": {}}, wantAllowed: false},
		{name: "mixed granted", any: []string{"a", "b"}, all: []string{"c"}, granted: map[string]struct{}{"b": {}, "c": {}}, wantAllowed: true},
		{name: "mixed missing any", any: []string{"a", "b"}, all: []string{"c"}, granted: map[string]struct{}{"c": {}}, wantAllowed: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			anyGranted := len(test.any) == 0
			for _, permission := range test.any {
				_, granted := test.granted[permission]
				anyGranted = anyGranted || granted
			}
			allGranted := true
			for _, permission := range test.all {
				_, granted := test.granted[permission]
				allGranted = allGranted && granted
			}
			if got := anyGranted && allGranted; got != test.wantAllowed {
				t.Fatalf("composite result=%v, want %v", got, test.wantAllowed)
			}
		})
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
