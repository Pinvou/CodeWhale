package account

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

// Unused concrete repository methods have no pool and would panic if a disabled
// route reached them. The actual production router, middleware and handlers run.
// This controlled authority fixture does not execute PostgreSQL or deployment.
type organizationProfileRepository struct {
	*PostgresRepository
	contextCalls, roleCalls, organizationCalls, mutationCalls, authorizationCalls int
	deny                                                                          bool
}

func (*organizationProfileRepository) GetAuthorizationVersions(context.Context, identity.Principal) (AuthorizationVersions, error) {
	return AuthorizationVersions{ContextVersion: 7, AuthorizationVersion: 3}, nil
}
func (r *organizationProfileRepository) Authorize(_ context.Context, _ identity.Principal, permission string, _ *uuid.UUID, _ *uuid.UUID) (AuthorizationDecision, error) {
	r.authorizationCalls++
	return AuthorizationDecision{Allowed: !r.deny, Permission: permission, ContextVersion: 7, AuthorizationVersion: 3}, nil
}
func (*organizationProfileRepository) RecordPermissionDenial(context.Context, identity.Principal, string, string) error {
	return nil
}
func (r *organizationProfileRepository) ListMyRoleBindings(context.Context, identity.Principal) ([]RoleBinding, error) {
	r.roleCalls++
	return []RoleBinding{}, nil
}
func (r *organizationProfileRepository) ListOrganizations(context.Context, identity.Principal) ([]Organization, error) {
	r.organizationCalls++
	return []Organization{}, nil
}
func (r *organizationProfileRepository) SetActiveContext(_ context.Context, _ identity.Principal, org *uuid.UUID, version int64) (Context, error) {
	r.contextCalls++
	r.mutationCalls++
	kind := "PERSONAL"
	if org != nil {
		kind = "ORGANIZATION"
	}
	return Context{Type: kind, OrganizationID: org, ContextVersion: version + 1, AuthorizationVersion: 3}, nil
}
func (r *organizationProfileRepository) AdminCreateOrganization(context.Context, identity.Principal, uuid.UUID, string, string) (Organization, error) {
	r.mutationCalls++
	return Organization{}, ErrConflict
}
func organizationProfileRequest(handler http.Handler, method, path, body string, authenticated bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if authenticated {
		request.Header.Set("Authorization", "Bearer controlled-user")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
func organizationProfileHandler(repo *organizationProfileRepository, enabled bool) *httpserver.Router {
	return NewService(repo, nil).WithOrganizationsEnabled(enabled).Handler(func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "controlled", Subject: "user", SessionID: "session"}, nil
	}).(*httpserver.Router)
}

func TestPersonalOrganizationRoutesCloseBeforeOrganizationOperations(t *testing.T) {
	repo := &organizationProfileRepository{}
	handler := organizationProfileHandler(repo, false)
	// Enumerate installed routes rather than using a string-only source fixture.
	checked := 0
	for _, route := range handler.Routes() {
		path := route.Path
		organizationRoute := strings.Contains(path, "/organizations") || strings.Contains(path, "/invitations") || strings.Contains(path, "/memberships")
		if !organizationRoute {
			continue
		}
		for _, segment := range strings.Split(path, "/") {
			if strings.HasPrefix(segment, ":") {
				path = strings.ReplaceAll(path, segment, uuid.NewString())
			}
		}
		t.Run(route.Method+" "+route.Path, func(t *testing.T) {
			response := organizationProfileRequest(handler, route.Method, path, `{}`, true)
			// HEAD carries no body, but the same availability middleware must run.
			if response.Code != http.StatusServiceUnavailable || (route.Method != http.MethodHead && !strings.Contains(response.Body.String(), `"code":"ORGANIZATION_UNAVAILABLE"`)) {
				t.Fatalf("disabled producer status=%d body=%s", response.Code, response.Body.String())
			}
		})
		checked++
	}
	if checked == 0 || repo.mutationCalls != 0 || repo.organizationCalls != 0 || repo.authorizationCalls != 0 {
		t.Fatalf("disabled routes=%d mutation=%d organization=%d permission=%d", checked, repo.mutationCalls, repo.organizationCalls, repo.authorizationCalls)
	}
	response := organizationProfileRequest(handler, http.MethodPost, "/admin/v1/organizations", `{}`, false)
	if response.Code != http.StatusUnauthorized {
		t.Fatal("availability gate displaced authentication", response.Code)
	}
}

func TestPersonalOrganizationProfileKeepsGenericIdentityRoutes(t *testing.T) {
	repo := &organizationProfileRepository{}
	handler := organizationProfileHandler(repo, false)
	response := organizationProfileRequest(handler, http.MethodGet, "/v1/me/role-bindings", "", true)
	if response.Code != http.StatusOK || repo.roleCalls != 1 {
		t.Fatal("personal role query unavailable", response.Code, repo.roleCalls)
	}
	response = organizationProfileRequest(handler, http.MethodPost, "/v1/sessions/current/active-context", `{"context_type":"PERSONAL","expected_context_version":7}`, true)
	if response.Code != http.StatusOK || repo.contextCalls != 1 || response.Header().Get("X-Context-Version") != "8" {
		t.Fatal("personal context unavailable", response.Code, repo.contextCalls, response.Header())
	}
	response = organizationProfileRequest(handler, http.MethodPost, "/v1/sessions/current/active-context", `{"context_type":"ORGANIZATION","organization_id":"`+uuid.NewString()+`","expected_context_version":7}`, true)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"ORGANIZATION_UNAVAILABLE"`) || repo.contextCalls != 1 {
		t.Fatal("organization context reached repository", response.Code, repo.contextCalls)
	}
	for _, body := range []string{`{"context_type":"PERSONAL","organization_id":"` + uuid.NewString() + `","expected_context_version":7}`, `{"context_type":"UNKNOWN","expected_context_version":7}`, `{"context_type":"PERSONAL","expected_context_version":0}`} {
		response = organizationProfileRequest(handler, http.MethodPost, "/v1/sessions/current/active-context", body, true)
		if response.Code != http.StatusBadRequest || repo.contextCalls != 1 {
			t.Fatal("invalid context mutated", response.Code, repo.contextCalls)
		}
	}
	repo.deny = true
	for _, item := range []struct{ method, path, body string }{{http.MethodGet, "/v1/me/role-bindings", ""}, {http.MethodPost, "/v1/sessions/current/active-context", `{"context_type":"PERSONAL","expected_context_version":7}`}} {
		response = organizationProfileRequest(handler, item.method, item.path, item.body, true)
		if response.Code != http.StatusForbidden || repo.roleCalls != 1 || repo.contextCalls != 1 {
			t.Fatal("generic permission was relaxed", response.Code)
		}
	}
}

func TestModelsOrganizationProfilePreservesInstalledRoutes(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		repo := &organizationProfileRepository{}
		service := NewService(repo, nil)
		if explicit {
			service.WithOrganizationsEnabled(true)
		}
		handler := service.Handler(func(context.Context, string) (identity.Principal, error) {
			return identity.Principal{Issuer: "controlled", Subject: "user", SessionID: "session"}, nil
		})
		response := organizationProfileRequest(handler, http.MethodGet, "/v1/me/organizations", "", true)
		if response.Code != http.StatusOK || repo.organizationCalls != 1 {
			t.Fatal("models route lost", explicit, response.Code)
		}
		response = organizationProfileRequest(handler, http.MethodPost, "/v1/sessions/current/active-context", `{"context_type":"ORGANIZATION","organization_id":"`+uuid.NewString()+`","expected_context_version":7}`, true)
		if response.Code != http.StatusOK || repo.contextCalls != 1 {
			t.Fatal("models context lost", explicit, response.Code)
		}
	}
}
