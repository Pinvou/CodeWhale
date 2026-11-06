package identityadapter

import "testing"

func TestNewServiceAcceptsWorkloadIdentityWithoutLegacyToken(t *testing.T) {
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "https://keycloak.example/realms/workload")
	t.Setenv("WORKLOAD_IDENTITY_JWKS_URL", "https://keycloak.example/realms/workload/protocol/openid-connect/certs")
	t.Setenv("WORKLOAD_IDENTITY_AUDIENCE", "pinvou-account-platform-internal")
	t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", `[{"subject":"fixture","service":"fixture","permissions":["internal.identity.user.create"]}]`)
	service, err := NewService(&PasswordVault{}, "")
	if err != nil || service == nil {
		t.Fatalf("workload-only identity service configuration was rejected: %v", err)
	}
}

func TestNewServiceRejectsMissingAuthenticationConfiguration(t *testing.T) {
	for _, name := range []string{"WORKLOAD_IDENTITY_ISSUER", "WORKLOAD_IDENTITY_JWKS_URL", "WORKLOAD_IDENTITY_AUDIENCE", "WORKLOAD_IDENTITY_RULES_JSON"} {
		t.Setenv(name, "")
	}
	if _, err := NewService(&PasswordVault{}, ""); err == nil {
		t.Fatal("missing legacy and workload authentication configuration was accepted")
	}
}

func TestNewServiceRejectsPartialWorkloadIdentityConfiguration(t *testing.T) {
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "https://keycloak.example/realms/workload")
	t.Setenv("WORKLOAD_IDENTITY_JWKS_URL", "")
	t.Setenv("WORKLOAD_IDENTITY_AUDIENCE", "pinvou-account-platform-internal")
	t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", `[{"subject":"fixture","service":"fixture","permissions":["internal.identity.user.create"]}]`)
	if _, err := NewService(&PasswordVault{}, ""); err == nil {
		t.Fatal("partial workload authentication configuration was accepted")
	}
}
