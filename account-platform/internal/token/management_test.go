package token

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestNormalizeMemberIDsRemovesNilAndDuplicates(t *testing.T) {
	first, second := uuid.New(), uuid.New()
	items := normalizeMemberIDs([]uuid.UUID{second, uuid.Nil, first, second})
	if len(items) != 2 || items[0].String() > items[1].String() {
		t.Fatalf("unexpected normalized members: %v", items)
	}
}

func TestOrdinaryTokenManagementRoutesAreNotRegistered(t *testing.T) {
	service := NewService(nil, nil, nil, nil, ProviderURLPolicy{}, nil, nil, 0)
	accountID, credentialID := uuid.New(), uuid.New()
	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/token-accounts"},
		{http.MethodPost, "/v1/token-accounts"},
		{http.MethodPatch, "/v1/token-accounts/" + accountID.String() + "/status"},
		{http.MethodPut, "/v1/token-accounts/" + accountID.String() + "/sharing"},
		{http.MethodPut, "/v1/token-accounts/" + accountID.String() + "/routes"},
		{http.MethodGet, "/v1/token-accounts/" + accountID.String() + "/credentials"},
		{http.MethodPost, "/v1/token-accounts/" + accountID.String() + "/credentials"},
		{http.MethodPost, "/v1/token-accounts/" + accountID.String() + "/credentials/" + credentialID.String() + "/revoke"},
		{http.MethodGet, "/v1/token-accounts/eligible?model=example"},
	}
	for _, item := range cases {
		request := httptest.NewRequest(item.method, item.path, strings.NewReader(`{"secret":"must-not-be-decoded"}`))
		request.Header.Set("Authorization", "Bearer oidc-token")
		response := httptest.NewRecorder()
		service.Handler("service-token", successfulBillingVerify).ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s %s returned %d, want no registered route", item.method, item.path, response.Code)
		}
	}
}

func TestManagementScopeBoundaries(t *testing.T) {
	if shareScopeAllowed("ORG", "OWNER_SUBTREE") {
		t.Fatal("organization-owned account cannot use a unit subtree without an owner unit")
	}
	if !shareScopeAllowed("ORG_UNIT", "OWNER_SUBTREE") {
		t.Fatal("organization-unit account should support subtree sharing")
	}
	if defaultScopeAllowed("USER", "ORG") || defaultScopeAllowed("ORG", "UNIT") {
		t.Fatal("default route scope must match the token owner boundary")
	}
}
