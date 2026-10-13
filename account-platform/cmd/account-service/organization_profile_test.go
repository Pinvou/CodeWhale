package main

import (
	"context"
	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOrganizationProfileAssembly(t *testing.T) {
	for _, tc := range []struct {
		name, profile, billing, token, credential string
		valid                                     bool
	}{
		{"personal-no-model-endpoints", "personal", "", "", "controlled-workload", true},
		{"personal-ignores-model-endpoint-inputs", " personal ", "invalid", "invalid", "controlled-workload", true},
		{"models-complete", "models", "http://billing-service:8080", "http://token-gateway:8080", "controlled-workload", true},
		{"default-models-complete", "", "http://billing-service:8080", "http://token-gateway:8080", "controlled-workload", true},
		{"models-missing-billing", "models", "", "http://token-gateway:8080", "controlled-workload", false},
		{"models-missing-token", "models", "http://billing-service:8080", "", "controlled-workload", false},
		{"default-never-personal-fallback", "", "http://billing-service:8080", "", "controlled-workload", false},
		{"models-missing-credential", "models", "http://billing-service:8080", "http://token-gateway:8080", "", false},
		{"unknown-profile", "unknown", "http://billing-service:8080", "http://token-gateway:8080", "controlled-workload", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ACCOUNT_PLATFORM_PROFILE", tc.profile)
			t.Setenv("BILLING_HTTP_URL", tc.billing)
			t.Setenv("TOKEN_HTTP_URL", tc.token)
			t.Setenv("TOKEN_GATEWAY_HTTP_URL", "http://obsolete-alias:8080")
			err := configureOrganizationLifecycle(account.NewService(nil, nil), tc.credential)
			if (err == nil) != tc.valid {
				t.Fatalf("profile %q valid=%v err=%v", tc.profile, tc.valid, err)
			}
		})
	}
}

// Exercise the real configuration result through the real Handler. This does
// not run main, PostgreSQL, OIDC discovery or a deployment.
type organizationProfileAssemblyRepository struct {
	*account.PostgresRepository
	organizationCalls, roleCalls int
}

func (*organizationProfileAssemblyRepository) GetAuthorizationVersions(context.Context, identity.Principal) (account.AuthorizationVersions, error) {
	return account.AuthorizationVersions{ContextVersion: 1}, nil
}
func (*organizationProfileAssemblyRepository) Authorize(_ context.Context, _ identity.Principal, permission string, _ *uuid.UUID, _ *uuid.UUID) (account.AuthorizationDecision, error) {
	return account.AuthorizationDecision{Allowed: true, Permission: permission}, nil
}
func (r *organizationProfileAssemblyRepository) ListOrganizations(context.Context, identity.Principal) ([]account.Organization, error) {
	r.organizationCalls++
	return []account.Organization{}, nil
}
func (r *organizationProfileAssemblyRepository) ListMyRoleBindings(context.Context, identity.Principal) ([]account.RoleBinding, error) {
	r.roleCalls++
	return []account.RoleBinding{}, nil
}
func TestOrganizationProfileConfiguredHandler(t *testing.T) {
	for _, profile := range []string{"personal", "models"} {
		t.Run(profile, func(t *testing.T) {
			t.Setenv("ACCOUNT_PLATFORM_PROFILE", profile)
			billing, token := "", ""
			if profile == "models" {
				billing = "http://billing-service:8080"
				token = "http://token-gateway:8080"
			}
			t.Setenv("BILLING_HTTP_URL", billing)
			t.Setenv("TOKEN_HTTP_URL", token)
			repo := &organizationProfileAssemblyRepository{}
			service := account.NewService(repo, nil)
			if err := configureOrganizationLifecycle(service, "controlled-workload"); err != nil {
				t.Fatal(err)
			}
			handler := service.Handler(func(context.Context, string) (identity.Principal, error) {
				return identity.Principal{Issuer: "controlled", Subject: "user", SessionID: "session"}, nil
			})
			request := httptest.NewRequest(http.MethodGet, "/v1/me/organizations", nil)
			request.Header.Set("Authorization", "Bearer controlled-user")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if profile == "personal" {
				if response.Code != http.StatusServiceUnavailable || repo.organizationCalls != 0 || !strings.Contains(response.Body.String(), `"code":"ORGANIZATION_UNAVAILABLE"`) {
					t.Fatal("personal config left producer available", response.Code, repo.organizationCalls)
				}
			} else if response.Code != http.StatusOK || repo.organizationCalls != 1 {
				t.Fatal("models config lost producer", response.Code, repo.organizationCalls)
			}
			request = httptest.NewRequest(http.MethodGet, "/v1/me/role-bindings", nil)
			request.Header.Set("Authorization", "Bearer controlled-user")
			response = httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK || repo.roleCalls != 1 {
				t.Fatal("generic role binding lost", profile, response.Code, repo.roleCalls)
			}
		})
	}
}
