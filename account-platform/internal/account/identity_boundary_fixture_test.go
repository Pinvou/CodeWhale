package account

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/identityadapter"
)

// Exercise Account's real HTTP client and the real Identity vault/directory.
// Only the remote Keycloak HTTP endpoint is a controlled, owned fixture.
func newIdentityBoundaryForTest(t *testing.T, keycloakURL, serviceToken string) (*HTTPIdentityProvisioner, *identityadapter.PasswordVault) {
	t.Helper()
	directory, err := identityadapter.NewKeycloakDirectory("test", keycloakURL, keycloakURL+"/token", "pinvou", "controlled-client", serviceToken)
	if err != nil {
		t.Fatal(err)
	}
	vault, err := identityadapter.NewPasswordVault([]byte(strings.Repeat("v", 32)), directory)
	if err != nil {
		t.Fatal(err)
	}
	service, err := identityadapter.NewService(vault, serviceToken)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(service.Handler())
	t.Cleanup(server.Close)
	client, err := NewHTTPIdentityProvisioner(server.URL, serviceToken)
	if err != nil {
		t.Fatal(err)
	}
	return client, vault
}

func registrationTokensForTest(t *testing.T) *RegistrationTokenSigner {
	t.Helper()
	tokens, err := NewRegistrationTokenSigner([]byte(strings.Repeat("q", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return tokens
}
