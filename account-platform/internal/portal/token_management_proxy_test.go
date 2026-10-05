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
			} {
				before := calls.Load()
				if status := request(denied.method, denied.path, true, true); status != http.StatusNotFound || calls.Load() != before {
					t.Fatal("undocumented method/path reached management upstream")
				}
			}
		})
	}
}
