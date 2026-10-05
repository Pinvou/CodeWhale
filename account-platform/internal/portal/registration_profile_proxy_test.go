package portal

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Real BFF/outbound HTTP with controlled sessions and provider responses; not
// Keycloak, Account authorization or database acceptance.
func TestPortalProfileRoleAndApprovalProxyPreservesPeerAndSecurity(t *testing.T) {
	const id = "4c947809-a2e8-4ef4-851b-f424984d1825"
	routes := []struct{ method, prefix, path string }{
		{"GET", "/api/account", "/v1/me/role-bindings"},
		{"POST", "/api/account", "/v1/me/profile-change-requests"},
		{"GET", "/api/account", "/v1/me/profile-change-requests/" + id},
		{"POST", "/api/account", "/v1/me/profile-change-requests/" + id + "/mobile-challenges"},
		{"POST", "/api/account", "/v1/me/profile-change-requests/" + id + "/actions/complete"},
		{"GET", "/api/admin/account", "/admin/v1/role-bindings"},
		{"POST", "/api/admin/account", "/admin/v1/role-bindings"},
		{"DELETE", "/api/admin/account", "/admin/v1/role-bindings/" + id},
		{"GET", "/api/admin/account", "/admin/v1/registration-operations"},
		{"POST", "/api/admin/account", "/admin/v1/registration-operations/" + id + "/actions/retry"},
		{"POST", "/api/admin/account", "/admin/v1/registration-operations/" + id + "/actions/compensate"},
		{"GET", "/api/admin/account", "/admin/v1/approval-requests"},
		{"POST", "/api/admin/account", "/admin/v1/approval-requests"},
		{"PUT", "/api/admin/account", "/admin/v1/approval-requests/" + id},
		{"POST", "/api/admin/account", "/admin/v1/approval-requests/" + id + "/actions/submit"},
		{"POST", "/api/admin/account", "/admin/v1/approval-requests/" + id + "/actions/withdraw"},
		{"POST", "/api/admin/account", "/admin/v1/approval-requests/" + id + "/actions/decide"},
	}
	store := newMemoryStore()
	service := testService(t, store)
	service.config.SourceAuthorizationToken = "controlled-internal-source-proof"
	session := Session{ID: "owned-session", AccessToken: "controlled-token", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour), CSRFToken: strings.Repeat("c", 32)}
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal("save fixture session")
	}
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("X-Pinvou-Client-Source") != "127.0.0.1" || r.Header.Get("X-Pinvou-Client-Source-Authorization") != service.config.SourceAuthorizationToken || r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Real-IP") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Device-ID") != fmt.Sprintf("%x", sha256.Sum256([]byte("pinvou-portal-device:"+session.ID))) || r.Header.Get("Authorization") != "Bearer "+session.AccessToken {
			t.Error("peer/credential boundary changed")
		}
		if r.Header.Get("Idempotency-Key") != "owned-operation-key" {
			t.Error("logical key lost")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"method": r.Method, "path": r.URL.Path})
	}))
	defer upstream.Close()
	service.config.AccountURL = parseTestURL(t, upstream.URL)
	portal := httptest.NewServer(service.Handler(nil))
	defer portal.Close()
	request := func(method, path string, authenticated, csrf bool) int {
		r, err := http.NewRequest(method, portal.URL+path, strings.NewReader(`{"fixture":true}`))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("X-Forwarded-For", "192.0.2.20")
		r.Header.Set("X-Device-ID", "untrusted-browser-device")
		r.Header.Set("X-Real-IP", "198.51.100.9")
		r.Header.Set("X-Pinvou-Client-Source", "203.0.113.50")
		r.Header.Set("Idempotency-Key", "owned-operation-key")
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
			t.Fatal("fixture HTTP failed")
		}
		defer response.Body.Close()
		if response.StatusCode == 200 {
			var received struct{ Method, Path string }
			if err := json.NewDecoder(response.Body).Decode(&received); err != nil || received.Method != method || !strings.HasSuffix(path, received.Path) {
				t.Fatal("method/path lost")
			}
		}
		return response.StatusCode
	}
	for _, route := range routes {
		t.Run(route.method+route.path, func(t *testing.T) {
			before := calls.Load()
			if status := request(route.method, route.prefix+route.path, true, true); status != 200 || calls.Load() != before+1 {
				t.Fatalf("allowed proxy status=%d", status)
			}
			before = calls.Load()
			if status := request(route.method, route.prefix+route.path, false, true); status != 401 || calls.Load() != before {
				t.Fatal("unauthenticated reached upstream")
			}
			if route.method != "GET" {
				if status := request(route.method, route.prefix+route.path, true, false); status != 403 || calls.Load() != before {
					t.Fatal("mutation bypassed CSRF")
				}
			}
		})
	}
	// Account retains its existing authenticated /v1 proxy surface: UUID and
	// action authorization remain Account handler responsibilities. Admin has
	// the existing explicit allowlist and must not expose arbitrary actions.
	for _, path := range []string{"/api/admin/account/admin/v1/approval-requests/" + id + "/actions/execute", "/api/admin/account/internal/v1/approval-requests"} {
		before := calls.Load()
		if status := request("POST", path, true, true); status != 404 || calls.Load() != before {
			t.Fatal("undocumented route reached upstream")
		}
	}
}

func TestPortalSourceProofIsOnlySentToConfiguredAccountAndNeverRedirected(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	service.config.SourceAuthorizationToken = "controlled-source-proof"
	session := Session{ID: "proof-session", AccessToken: "controlled-token", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour), CSRFToken: strings.Repeat("c", 32)}
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal("save fixture")
	}
	var targetCalls atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalls.Add(1)
		if r.Header.Get("X-Pinvou-Client-Source-Authorization") != "" {
			t.Error("proof leaked outside Account")
		}
		w.WriteHeader(200)
	}))
	defer other.Close()
	account := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Pinvou-Client-Source-Authorization") != service.config.SourceAuthorizationToken {
			t.Error("missing Account source proof")
		}
		http.Redirect(w, r, other.URL, 302)
	}))
	defer account.Close()
	service.config.AccountURL = parseTestURL(t, account.URL)
	service.config.TokenGatewayURL = parseTestURL(t, other.URL)
	portal := httptest.NewServer(service.Handler(nil))
	defer portal.Close()
	for _, path := range []string{"/api/account/v1/me/role-bindings", "/api/token/v1/me/balance"} {
		r, _ := http.NewRequest("GET", portal.URL+path, nil)
		r.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
		r.Header.Set("X-Pinvou-Client-Source-Authorization", "untrusted-browser-proof")
		response, err := portal.Client().Do(r)
		if err != nil {
			t.Fatal("owned HTTP")
		}
		response.Body.Close()
	}
	if targetCalls.Load() != 1 {
		t.Fatal("redirect was followed")
	}
}

func TestPortalLogoutAfterProfileRevocationDoesNotRefreshAndClearsBothCookies(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	// A confirmed profile change has revoked provider tokens. Deleting the
	// existing BFF session must not require exchanging those tokens again.
	session := Session{ID: "profile-owned", AccessToken: "revoked-controlled", RefreshToken: "revoked-controlled", AccessExpiresAt: time.Now().Add(-time.Minute), RefreshExpiresAt: time.Now().Add(time.Hour), CSRFToken: strings.Repeat("c", 32)}
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/logout", nil)
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
	request.AddCookie(&http.Cookie{Name: service.csrfCookieName(), Value: session.CSRFToken})
	request.Header.Set("X-CSRF-Token", session.CSRFToken)
	request.Header.Set("Origin", service.config.PublicURL.String())
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("logout status %d", response.Code)
	}
	if _, err := store.GetSession(context.Background(), session.ID); err == nil {
		t.Fatal("session retained after logout")
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 2 {
		t.Fatal("logout did not expire both cookies")
	}
	for _, cookie := range cookies {
		if cookie.MaxAge >= 0 {
			t.Fatal("logout cookie was not expired")
		}
	}
}
