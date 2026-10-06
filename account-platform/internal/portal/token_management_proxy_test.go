package portal

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise the production BFF router and outbound HTTP client. Token ownership
// authorization remains upstream; these controlled sessions/responses are not
// Keycloak, Token database or provider acceptance tests.
func TestPortalPreservesUserAndOrganizationTokenManagementHTTP(t *testing.T) {
	routes := []struct{ method, path string }{
		{http.MethodGet, "/v1/token-accounts"},
		{http.MethodPost, "/v1/token-accounts"},
		{http.MethodPatch, "/v1/token-accounts/owned-account/status"},
		{http.MethodPut, "/v1/token-accounts/owned-account/sharing"},
		{http.MethodPut, "/v1/token-accounts/owned-account/routes"},
		{http.MethodGet, "/v1/token-accounts/owned-account/credentials"},
		{http.MethodPost, "/v1/token-accounts/owned-account/credentials"},
		{http.MethodPost, "/v1/token-accounts/owned-account/credentials/owned-credential/revoke"},
		{http.MethodGet, "/v1/token-accounts/eligible"},
		{http.MethodGet, "/v1/usage-requests/owned-usage"},
		{http.MethodPost, "/v1/recharge/orders/owned-order/close"},
		{http.MethodGet, "/v1/organizations/owned-org/recharge/products"},
		{http.MethodGet, "/v1/organizations/owned-org/entitlement-packages"},
		{http.MethodGet, "/v1/organizations/owned-org/quota-grants"},
		{http.MethodPost, "/v1/organizations/owned-org/quota-grants:batch"},
		{http.MethodPost, "/v1/organizations/owned-org/quota-grants/owned-grant/revoke"},
		{http.MethodPost, "/v1/organizations/owned-org/recharge/orders"},
	}
	for _, owner := range []string{"personal-user", "organization-owner"} {
		t.Run(owner, func(t *testing.T) {
			store := newMemoryStore()
			service := testService(t, store)
			session := Session{ID: "controlled-session", AccessToken: owner, AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour), CSRFToken: strings.Repeat("c", 32)}
			if err := store.SaveSession(context.Background(), session); err != nil {
				t.Fatal("save controlled session")
			}
			var calls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Authorization") != "Bearer "+session.AccessToken || r.Header.Get("Cookie") != "" || r.Header.Get("X-CSRF-Token") != "" || r.URL.RawQuery != "model=owned-test-model" {
					t.Error("upstream identity/query or browser-secret boundary changed")
				}
				if r.Header.Get("X-Forwarded-Forbidden") != "" {
					t.Error("browser-only header forwarded")
				}
				if r.URL.Path == "/v1/token-accounts/forbidden-account/credentials" {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				_, _ = io.ReadAll(r.Body)
				w.Header().Set("X-Context-Version", "17")
				w.Header().Set("X-Authorization-Version", "23")
				w.Header().Set("X-Internal-Secret", "controlled-secret-not-forwarded")
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]string{"method": r.Method, "path": r.URL.Path})
			}))
			defer upstream.Close()
			service.config.TokenGatewayURL = parseTestURL(t, upstream.URL)
			portal := httptest.NewServer(service.Handler(nil))
			defer portal.Close()
			request := func(method, resource string, authenticated, csrf bool) int {
				r, err := http.NewRequest(method, portal.URL+"/api/token"+resource+"?model=owned-test-model", strings.NewReader(`{"fixture":true}`))
				if err != nil {
					t.Fatal("create controlled HTTP request")
				}
				if authenticated {
					r.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
				}
				if csrf {
					r.Header.Set("Origin", "https://portal.example")
					r.Header.Set("X-CSRF-Token", session.CSRFToken)
					r.AddCookie(&http.Cookie{Name: service.csrfCookieName(), Value: session.CSRFToken})
				}
				response, err := portal.Client().Do(r)
				if err != nil {
					t.Fatal("controlled HTTP request failed")
				}
				defer response.Body.Close()
				if response.StatusCode == http.StatusOK {
					if response.Header.Get("X-Context-Version") != "17" || response.Header.Get("X-Authorization-Version") != "23" || response.Header.Get("X-Internal-Secret") != "" {
						t.Fatal("context versions or private header boundary changed")
					}
					var received struct{ Method, Path string }
					if err := json.NewDecoder(response.Body).Decode(&received); err != nil || received.Method != method || received.Path != resource {
						t.Fatal("management method/path was not preserved end to end")
					}
				} else {
					_, _ = io.Copy(io.Discard, response.Body)
				}
				return response.StatusCode
			}
			for _, route := range routes {
				t.Run(route.method+route.path, func(t *testing.T) {
					before := calls.Load()
					if status := request(route.method, route.path, true, true); status != http.StatusOK || calls.Load() != before+1 {
						t.Fatal("documented management route did not reach upstream once")
					}
					before = calls.Load()
					if status := request(route.method, route.path, false, true); status != http.StatusUnauthorized || calls.Load() != before {
						t.Fatal("unauthenticated request reached management upstream")
					}
					if route.method != http.MethodGet {
						if status := request(route.method, route.path, true, false); status != http.StatusForbidden || calls.Load() != before {
							t.Fatal("management mutation bypassed CSRF")
						}
					}
				})
			}
			if status := request(http.MethodGet, "/v1/token-accounts/forbidden-account/credentials", true, true); status != http.StatusForbidden {
				t.Fatal("upstream owner denial was not retained")
			}
			for _, denied := range []struct{ method, path string }{
				{http.MethodPost, "/v1/chat/completions"},
				{http.MethodGet, "/v1/chat/completions"},
				{http.MethodGet, "/admin/v1/token-accounts"},
				{http.MethodGet, "/internal/v1/token-accounts"},
				{http.MethodDelete, "/v1/token-accounts/owned-account"},
				{http.MethodPost, "/v1/token-accounts/owned-account/status"},
				{http.MethodPatch, "/v1/token-accounts/owned-account/sharing"},
				{http.MethodGet, "/v1/token-accounts/owned-account/credentials/owned-credential"},
				{http.MethodPost, "/v1/token-accounts/owned-account/credentials/owned-credential/revoke/extra"},
				{http.MethodGet, "/v1/usage-requests/owned-usage/extra"},
				{http.MethodPost, "/v1/recharge/orders/owned-order/close/extra"},
				{http.MethodPost, "/v1/organizations/owned-org/recharge/products"},
				{http.MethodPost, "/v1/organizations/owned-org/entitlement-packages"},
				{http.MethodPost, "/v1/organizations/owned-org/quota-grants"},
				{http.MethodGet, "/v1/organizations/owned-org/quota-grants:batch"},
				{http.MethodGet, "/v1/organizations/owned-org/quota-grants/owned-grant/revoke"},
				{http.MethodPost, "/v1/organizations/owned-org/quota-grants/owned-grant/revoke/extra"},
				{http.MethodGet, "/v1/organizations/owned-org/recharge/products/extra"},
			} {
				before := calls.Load()
				if status := request(denied.method, denied.path, true, true); status != http.StatusNotFound || calls.Load() != before {
					t.Fatal("undocumented method/path reached management upstream")
				}
			}
		})
	}
}

func TestPortalGlobalContextPUTVersionHTTP(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	session := Session{ID: "controlled-global-session", AccessToken: "controlled-access", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour), CSRFToken: strings.Repeat("c", 32)}
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal("save controlled session")
	}
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			ContextType     string  `json:"context_type"`
			OrganizationID  *string `json:"organization_id"`
			ExpectedVersion int     `json:"expected_context_version"`
		}
		if r.Method != http.MethodPut || r.URL.Path != "/v1/me/active-context" || json.NewDecoder(r.Body).Decode(&body) != nil || body.ContextType != "PERSONAL" || body.OrganizationID != nil || body.ExpectedVersion != 8 {
			t.Error("formal global context PUT/CAS facts changed")
		}
		if r.Header.Get("Authorization") != "Bearer "+session.AccessToken || r.Header.Get("X-Pinvou-Client-Source-Authorization") != "controlled-internal-proof" || r.Header.Get("Cookie") != "" {
			t.Error("account identity/source proof boundary changed")
		}
		w.Header().Set("X-Context-Version", "9")
		w.Header().Set("X-Authorization-Version", "12")
		w.Header().Set("X-Internal-Secret", "controlled-private-header")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"context_type":"PERSONAL","organization_id":null,"context_version":9,"authorization_version":12,"permissions":[]}`))
	}))
	defer upstream.Close()
	service.config.AccountURL = parseTestURL(t, upstream.URL)
	service.config.SourceAuthorizationToken = "controlled-internal-proof"
	portal := httptest.NewServer(service.Handler(nil))
	defer portal.Close()
	for _, csrf := range []bool{false, true} {
		r, err := http.NewRequest(http.MethodPut, portal.URL+"/api/account/v1/me/active-context", strings.NewReader(`{"context_type":"PERSONAL","organization_id":null,"expected_context_version":8}`))
		if err != nil {
			t.Fatal("create controlled request")
		}
		r.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
		if csrf {
			r.Header.Set("Origin", "https://portal.example")
			r.Header.Set("X-CSRF-Token", session.CSRFToken)
			r.AddCookie(&http.Cookie{Name: service.csrfCookieName(), Value: session.CSRFToken})
		}
		response, err := portal.Client().Do(r)
		if err != nil {
			t.Fatal("controlled HTTP request failed")
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if !csrf && (response.StatusCode != http.StatusForbidden || calls.Load() != 0) {
			t.Fatal("PUT bypassed CSRF")
		}
		if csrf && (response.StatusCode != http.StatusOK || calls.Load() != 1 || response.Header.Get("X-Context-Version") != "9" || response.Header.Get("X-Authorization-Version") != "12" || response.Header.Get("X-Internal-Secret") != "") {
			t.Fatal("context PUT version or private header boundary changed")
		}
	}
}
