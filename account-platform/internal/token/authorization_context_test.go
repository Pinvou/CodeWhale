package token

import "testing"

func TestActiveScopeAuthorizationReadsThreeScopePersonalFields(t *testing.T) {
	roles, permissions := activeScopeAuthorization(
		"PERSONAL",
		[]string{},
		[]string{},
		[]string{"PERSONAL_USER"},
		[]string{"personal.models.read", "personal.model.invoke"},
	)
	if len(roles) != 1 || roles[0] != "PERSONAL_USER" {
		t.Fatalf("unexpected roles: %#v", roles)
	}
	if len(permissions) != 2 || permissions[1] != "personal.model.invoke" {
		t.Fatalf("unexpected permissions: %#v", permissions)
	}
}

func TestActiveScopeAuthorizationRetainsLegacyPersonalFields(t *testing.T) {
	roles, permissions := activeScopeAuthorization(
		"PERSONAL",
		[]string{"PERSONAL_USER"},
		[]string{"personal.model.invoke"},
		nil,
		nil,
	)
	if len(roles) != 1 || roles[0] != "PERSONAL_USER" || len(permissions) != 1 || permissions[0] != "personal.model.invoke" {
		t.Fatalf("legacy authorization changed: roles=%#v permissions=%#v", roles, permissions)
	}
}

func TestActiveScopeAuthorizationPreservesOrganizationFields(t *testing.T) {
	roles, permissions := activeScopeAuthorization(
		"ORGANIZATION",
		[]string{"MEMBER"},
		[]string{"org.model.invoke"},
		[]string{"PERSONAL_USER"},
		[]string{"personal.model.invoke"},
	)
	if len(roles) != 1 || roles[0] != "MEMBER" || len(permissions) != 1 || permissions[0] != "org.model.invoke" {
		t.Fatalf("organization authorization changed: roles=%#v permissions=%#v", roles, permissions)
	}
}
