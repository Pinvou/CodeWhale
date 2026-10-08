package token

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/google/uuid"
)

func newModelUsageAuthorizationService(t *testing.T, resolved AuthorizationContext) (*Service, *authorizationDecisionRecorder, func()) {
	t.Helper()
	recorder := &authorizationDecisionRecorder{allowed: true, userID: resolved.UserID}
	server := httptest.NewServer(recorder)
	authorizer, err := authorization.NewClient(server.URL, "service-token")
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	service := NewService(fixedBillingContextResolver{resolved: resolved}, nil, nil).WithAuthorizer(authorizer)
	service.personalExecutionEnabled = true
	service.executionUpstream = NewHTTPUpstreamClient()
	return service, recorder, server.Close
}

func TestModelAndUsageRoutesUseContextSpecificAtomicPermissions(t *testing.T) {
	userID, organizationID := uuid.New(), uuid.New()
	membershipVersion := int64(7)
	tests := []struct {
		name             string
		context          AuthorizationContext
		method           string
		path             string
		body             string
		permission       string
		organizationGate bool
		deniedContext    bool
	}{
		{name: "personal protocols", context: AuthorizationContext{UserID: userID, ContextType: "PERSONAL"}, method: http.MethodGet, path: "/v1/model-protocols", permission: "personal.models.read"},
		{name: "organization protocols", context: AuthorizationContext{UserID: userID, ContextType: "ORGANIZATION", OrganizationID: &organizationID, OrganizationMembershipVersion: &membershipVersion}, method: http.MethodGet, path: "/v1/model-protocols", permission: "org.models.read", organizationGate: true},
		{name: "personal invocation", context: AuthorizationContext{UserID: userID, ContextType: "PERSONAL"}, method: http.MethodPost, path: "/v1/chat/completions", body: `{}`, permission: "personal.model.invoke"},
		{name: "organization invocation", context: AuthorizationContext{UserID: userID, ContextType: "ORGANIZATION", OrganizationID: &organizationID, OrganizationMembershipVersion: &membershipVersion}, method: http.MethodPost, path: "/v1/chat/completions", body: `{}`, permission: "org.model.invoke", organizationGate: true},
		{name: "personal usage list", context: AuthorizationContext{UserID: userID, ContextType: "PERSONAL"}, method: http.MethodGet, path: "/v1/me/usage-requests?cursor=invalid", permission: "personal.usage.read"},
		{name: "organization usage list", context: AuthorizationContext{UserID: userID, ContextType: "ORGANIZATION", OrganizationID: &organizationID, OrganizationMembershipVersion: &membershipVersion}, method: http.MethodGet, path: "/v1/me/usage-requests?cursor=invalid", permission: "org.usage.read.self", organizationGate: true},
		{name: "personal usage turn projection", context: AuthorizationContext{UserID: userID, ContextType: "PERSONAL"}, method: http.MethodGet, path: "/v1/me/usage-turns", permission: "personal.usage.read"},
		{name: "organization usage turn projection", context: AuthorizationContext{UserID: userID, ContextType: "ORGANIZATION", OrganizationID: &organizationID, OrganizationMembershipVersion: &membershipVersion}, method: http.MethodGet, path: "/v1/me/usage-turns", permission: "org.usage.read.self", organizationGate: true},
		{name: "organization missing local membership version", context: AuthorizationContext{UserID: userID, ContextType: "ORGANIZATION", OrganizationID: &organizationID, MembershipVersion: 99}, method: http.MethodGet, path: "/v1/me/usage-turns", deniedContext: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, recorder, closeServer := newModelUsageAuthorizationService(t, test.context)
			defer closeServer()
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer oidc-token")
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			service.Handler(successfulBillingVerify).ServeHTTP(response, request)
			if test.deniedContext {
				if response.Code != http.StatusForbidden || len(recorder.permissions) != 0 || recorder.organizationID != nil {
					t.Fatalf("unavailable context dispatched authorization: status=%d permissions=%v organization=%v", response.Code, recorder.permissions, recorder.organizationID)
				}
				return
			}
			if len(recorder.permissions) != 1 || recorder.permissions[0] != test.permission {
				t.Fatalf("permissions=%v", recorder.permissions)
			}
			if test.organizationGate != (recorder.organizationID != nil && *recorder.organizationID == organizationID) {
				t.Fatalf("organization target=%v", recorder.organizationID)
			}
		})
	}
}

func TestCancellationUsesStablePersonalUserPermissionWithoutActiveOrganization(t *testing.T) {
	userID := uuid.New()
	service, recorder, closeServer := newModelUsageAuthorizationService(t, AuthorizationContext{UserID: userID, ContextType: "ORGANIZATION", OrganizationID: ptrUUID(uuid.New())})
	defer closeServer()
	request := httptest.NewRequest(http.MethodPost, "/v1/usage-requests/not-a-uuid/cancel", nil)
	request.Header.Set("Authorization", "Bearer oidc-token")
	response := httptest.NewRecorder()
	service.Handler(successfulBillingVerify).ServeHTTP(response, request)
	if len(recorder.permissions) != 1 || recorder.permissions[0] != "personal.usage.cancel" || recorder.organizationID != nil {
		t.Fatalf("unexpected cancellation authorization: %v / %v", recorder.permissions, recorder.organizationID)
	}
}

func ptrUUID(value uuid.UUID) *uuid.UUID { return &value }
