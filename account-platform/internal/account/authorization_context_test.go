package account

import "testing"

func TestNormalizeActiveScopeAuthorizationUsesPersonalProjection(t *testing.T) {
	resolved := AuthorizationContext{
		ContextType:         "PERSONAL",
		Roles:               []string{"SHOULD_NOT_SURVIVE"},
		Permissions:         []string{"org.model.invoke"},
		PersonalRoles:       []string{"PERSONAL_USER"},
		PersonalPermissions: []string{"personal.models.read", "personal.model.invoke"},
	}

	normalizeActiveScopeAuthorization(&resolved)

	if len(resolved.Roles) != 1 || resolved.Roles[0] != "PERSONAL_USER" {
		t.Fatalf("unexpected active roles: %#v", resolved.Roles)
	}
	if len(resolved.Permissions) != 2 || resolved.Permissions[1] != "personal.model.invoke" {
		t.Fatalf("unexpected active permissions: %#v", resolved.Permissions)
	}
}

func TestNormalizeActiveScopeAuthorizationPreservesOrganizationProjection(t *testing.T) {
	resolved := AuthorizationContext{
		ContextType:         "ORGANIZATION",
		Roles:               []string{"MEMBER"},
		Permissions:         []string{"org.model.invoke"},
		PersonalRoles:       []string{"PERSONAL_USER"},
		PersonalPermissions: []string{"personal.model.invoke"},
	}

	normalizeActiveScopeAuthorization(&resolved)

	if len(resolved.Roles) != 1 || resolved.Roles[0] != "MEMBER" || len(resolved.Permissions) != 1 || resolved.Permissions[0] != "org.model.invoke" {
		t.Fatalf("organization authorization changed: roles=%#v permissions=%#v", resolved.Roles, resolved.Permissions)
	}
}
