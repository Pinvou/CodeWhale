package token

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPContextResolverKeepsGlobalAndOrganizationMembershipVersionsDistinct(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user_id":"018f0000-0000-7000-8000-000000000001","session_id":"session","context_type":"ORGANIZATION","organization_id":"018f0000-0000-7000-8000-000000000002","membership_version":9,"organization_membership_version":3,"context_version":1,"authorization_version":1,"unit_ids":[],"ancestor_unit_ids":[],"roles":[],"permissions":[],"personal_roles":[],"personal_permissions":[],"unit_authorization_bindings":[],"mobile_verified":false}`))
	}))
	defer server.Close()
	resolver, err := NewHTTPContextResolver(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolver.Resolve(context.Background(), "Bearer test")
	if err != nil || resolved.MembershipVersion != 9 || resolved.OrganizationMembershipVersion == nil || *resolved.OrganizationMembershipVersion != 3 {
		t.Fatalf("resolved context = %#v, err=%v", resolved, err)
	}
}

func TestHTTPContextResolverFailsClosedWithoutOrganizationMembershipVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user_id":"018f0000-0000-7000-8000-000000000001","session_id":"session","context_type":"ORGANIZATION","organization_id":"018f0000-0000-7000-8000-000000000002","membership_version":9,"context_version":1,"authorization_version":1}`))
	}))
	defer server.Close()
	resolver, err := NewHTTPContextResolver(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(context.Background(), "Bearer test"); err != ErrOrganizationMembershipContext {
		t.Fatalf("missing organization membership version error = %v", err)
	}
}
