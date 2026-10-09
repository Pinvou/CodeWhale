package identityadapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestIndependentIdentityBadBodiesNeverACKSuccess(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	for _, n := range []string{"WORKLOAD_IDENTITY_ISSUER", "WORKLOAD_IDENTITY_CA_FILE", "WORKLOAD_IDENTITY_JWKS_URL", "WORKLOAD_IDENTITY_AUDIENCE", "WORKLOAD_IDENTITY_RULES_JSON"} {
		t.Setenv(n, "")
	}
	vault, err := NewPasswordVault([]byte(strings.Repeat("k", 32)), &directoryFake{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewService(vault, "independent-fixture-token")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ name, method, path, body string }{
		{"precheck-malformed", "POST", "/v1/registration/password-prechecks", "{"},
		{"validate-malformed", "POST", "/internal/v1/registration/password-references/validate", "{"},
		{"create-malformed", "POST", "/internal/v1/users/mobile", "{"},
		{"username-malformed", "PUT", "/internal/v1/users/fixture/username", "{"},
		{"mobile-malformed", "PUT", "/internal/v1/users/fixture/mobile", "{"},
		{"reset-malformed", "POST", "/internal/v1/users/fixture/password-resets", "{"},
		{"username-missing-command", "PUT", "/internal/v1/users/fixture/username", "{}"},
		{"mobile-missing-command", "PUT", "/internal/v1/users/fixture/mobile", "{}"},
		{"reset-missing-command", "POST", "/internal/v1/users/fixture/password-resets", "{}"},
	} {
		t.Run(item.name, func(t *testing.T) {
			r := httptest.NewRequest(item.method, item.path, strings.NewReader(item.body))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer independent-fixture-token")
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("invalid body acknowledged status=%d bytes=%d; require 400", w.Code, w.Body.Len())
			}
		})
	}
}

func TestIndependentDisableConfirmationRequiresActualEnabledField(t *testing.T) {
	for _, body := range []string{"{}", "null", "{\"enabled\":false} {}", "{\"enabled\":true}", "{\"enabled\":false}"} {
		name := map[string]string{"{}": "missing", "null": "null", "{\"enabled\":false} {}": "trailing", "{\"enabled\":true}": "active", "{\"enabled\":false}": "disabled"}[body]
		t.Run(name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path == "/token" {
					w.Write([]byte(`{"access_token":"fixture-token","expires_in":300}`))
					return
				}
				if r.Method == "GET" {
					w.Write([]byte(body))
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			a, _ := url.Parse(server.URL)
			u, _ := url.Parse(server.URL + "/token")
			d := &KeycloakDirectory{adminURL: a, tokenURL: u, realm: "pinvou", clientID: "fixture", clientSecret: "fixture", client: server.Client()}
			err := d.DisableUserAndConfirm(context.Background(), "fixture")
			if name == "disabled" {
				if err != nil {
					t.Fatalf("valid disabled proof rejected; calls=%d", calls)
				}
			} else if err == nil {
				t.Fatalf("invalid disable authority accepted; calls=%d", calls)
			}
		})
	}
}
