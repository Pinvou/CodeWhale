package portal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Production BFF and outbound HTTP with controlled domain replies, not PG or
// domain authorization acceptance.
func TestMeteringFinancialProxyPreservesSessionLogicalKeyAndFiniteRoutes(t *testing.T) {
	const id = "415768a7-47af-4d71-b75e-45f54fb5aa45"
	store := newMemoryStore()
	service := testService(t, store)
	service.config.SourceAuthorizationToken = "controlled-account-only-proof"
	session := Session{ID: "metering-session", AccessToken: "controlled-token", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour), CSRFToken: strings.Repeat("c", 32)}
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal("fixture session failed")
	}
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+session.AccessToken || r.Header.Get("Idempotency-Key") != id || r.Header.Get("Cookie") != "" || r.Header.Get("X-Pinvou-Client-Source-Authorization") != "" {
			t.Error("session/key/domain credential boundary changed")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("denied") == "true" {
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"error":{"code":"PERMISSION_DENIED"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"method": r.Method, "path": r.URL.Path})
	}))
	defer upstream.Close()
	service.config.BillingURL = parseTestURL(t, upstream.URL)
	service.config.TokenGatewayURL = parseTestURL(t, upstream.URL)
	portal := httptest.NewServer(service.Handler(nil))
	defer portal.Close()
	request := func(method, path string, authenticated, csrf bool) *http.Response {
		r, err := http.NewRequest(method, portal.URL+path, strings.NewReader(`{"controlled":true}`))
		if err != nil {
			t.Fatal("fixture request failed")
		}
		r.Header.Set("Idempotency-Key", id)
		r.Header.Set("X-Pinvou-Client-Source-Authorization", "untrusted-browser-proof")
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
		return response
	}
	routes := []struct{ method, area, path string }{
		{"GET", "billing", "/admin/v1/metering-units"}, {"POST", "billing", "/admin/v1/metering-units"},
		{"POST", "billing", "/admin/v1/pricing/drafts"}, {"PUT", "billing", "/admin/v1/pricing/" + id + "/draft"},
		{"POST", "billing", "/admin/v1/pricing/" + id + "/publish"}, {"POST", "billing", "/admin/v1/pricing/" + id + "/transition"},
		{"GET", "billing", "/admin/v1/payment-orders/" + id}, {"GET", "billing", "/admin/v1/usage-holds/" + id},
		{"GET", "billing", "/admin/v1/receivables/" + id + "/allocations"}, {"POST", "billing", "/admin/v1/late-payment-reviews/" + id + "/query"},
		{"POST", "billing", "/admin/v1/payment-anomalies/" + id + "/resolve"}, {"POST", "billing", "/admin/v1/reconciliation-cases/" + id + "/commands"},
		{"POST", "token", "/admin/v1/protocols"}, {"POST", "token", "/admin/v1/protocols/" + id + "/formulas"},
		{"POST", "token", "/admin/v1/protocols/" + id + "/publish"}, {"POST", "token", "/admin/v1/formulas/" + id + "/publish"},
		{"GET", "token", "/admin/v1/formulas/" + id + "/preview"},
	}
	for _, route := range routes {
		t.Run(route.method+route.path, func(t *testing.T) {
			path := "/api/admin/" + route.area + route.path
			before := calls.Load()
			response := request(route.method, path, true, true)
			var received struct{ Method, Path string }
			err := json.NewDecoder(response.Body).Decode(&received)
			response.Body.Close()
			if response.StatusCode != 200 || err != nil || received.Method != route.method || received.Path != route.path || calls.Load() != before+1 {
				t.Fatal("exact route was not forwarded")
			}
			before = calls.Load()
			response = request(route.method, path, false, true)
			response.Body.Close()
			if response.StatusCode != 401 || calls.Load() != before {
				t.Fatal("unauthenticated proxy reached upstream")
			}
			if route.method != "GET" {
				response = request(route.method, path, true, false)
				response.Body.Close()
				if response.StatusCode != 403 || calls.Load() != before {
					t.Fatal("CSRF-less mutation reached upstream")
				}
			}
		})
	}
	for _, path := range []string{"/api/admin/billing/admin/v1/late-payment-reviews/" + id + "/query", "/api/admin/token/admin/v1/formulas/" + id + "/publish"} {
		before := calls.Load()
		response := request("GET", path, true, true)
		response.Body.Close()
		if response.StatusCode != 404 || calls.Load() != before {
			t.Fatal("wrong method bypassed finite allowlist")
		}
	}
	response := request("POST", "/api/admin/token/admin/v1/protocols?denied=true", true, true)
	var denied struct{ Error struct{ Code string } }
	err := json.NewDecoder(response.Body).Decode(&denied)
	response.Body.Close()
	if response.StatusCode != 403 || err != nil || denied.Error.Code != "PERMISSION_DENIED" {
		t.Fatal("upstream permission denial changed")
	}
	for _, invalidOrderID := range []string{"invalid", uuid.Nil.String(), strings.ReplaceAll(id, "-", "")} {
		before := calls.Load()
		response := request("POST", "/api/token/v1/recharge/orders/"+invalidOrderID+"/payment-claims", true, true)
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound || calls.Load() != before {
			t.Fatal("invalid payment claim order must not be forwarded")
		}
	}
	response = request("POST", "/api/token/v1/recharge/orders/"+id+"/payment-claims", true, true)
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("owned-user payment claim did not reach Gateway")
	}
}
