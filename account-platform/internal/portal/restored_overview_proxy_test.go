package portal

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRestoredOverviewProxyAllowsOnlyExactReadRoutes(t *testing.T) {
	for _, allowed := range []func(string, string) bool{accountAdminPathAllowed, tokenAdminPathAllowed} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			if !allowed(method, "/admin/v1/overview") {
				t.Fatal("overview read route is missing")
			}
		}
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
			if allowed(method, "/admin/v1/overview") {
				t.Fatal("overview mutation forwarded")
			}
		}
		for _, path := range []string{"/admin/v1/overview/secret", "/admin/v2/overview", "/admin/v1/overview/../users"} {
			if allowed(http.MethodGet, path) {
				t.Fatal("noncanonical overview route forwarded")
			}
		}
	}
}

func TestRestoredTokenOverviewKeepsSessionAndSignedSource(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(map[bool]string{true: "signed", false: "missing authority"}[configured], func(t *testing.T) {
			store := newMemoryStore()
			service := testService(t, store)
			key := []byte("controlled-overview-source-key-32bytes")
			if configured {
				service.config.TokenAdminProxyHMACKey = key
			}
			session := validSession("controlled-overview-session")
			if err := store.SaveSession(context.Background(), session); err != nil {
				t.Fatal(err)
			}
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.RequestURI() != "/admin/v1/overview?original=1" || r.Header.Get("Authorization") != "Bearer "+session.AccessToken || r.Header.Get("Cookie") != "" {
					t.Error("session or URI authority changed")
				}
				if r.Header.Get("X-Pinvou-Client-Source") != "198.51.100.22" {
					t.Error("browser source accepted")
				}
				mac := hmac.New(sha256.New, key)
				_, _ = mac.Write([]byte("GET\n/admin/v1/overview?original=1\n198.51.100.22"))
				signature, err := hex.DecodeString(r.Header.Get("X-Pinvou-Client-Source-Signature"))
				if err != nil || !hmac.Equal(signature, mac.Sum(nil)) {
					t.Error("source signature mismatch")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"token_accounts":2}`))
			}))
			defer upstream.Close()
			service.config.TokenGatewayURL = parseTestURL(t, upstream.URL)
			request := financeBrowserRequest(service, session, http.MethodGet, "/api/admin/token/admin/v1/overview?original=1", "")
			request.RemoteAddr = "198.51.100.22:443"
			request.Header.Set("Authorization", "Bearer untrusted-browser")
			request.Header.Set("X-Forwarded-For", "203.0.113.55")
			request.Header.Set("X-Pinvou-Client-Source", "203.0.113.56")
			request.Header.Set("X-Pinvou-Client-Source-Signature", strings.Repeat("00", 32))
			response := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(response, request)
			if configured {
				if response.Code != 200 || calls != 1 || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("signed overview not delivered")
				}
			} else if response.Code != 503 || calls != 0 {
				t.Fatal("unsigned overview was forwarded")
			}
			request = httptest.NewRequest(http.MethodGet, "/api/admin/token/admin/v1/overview", nil)
			response = httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized {
				t.Fatal("missing session not denied")
			}
		})
	}
}

func TestRestoredFinancePricingOverviewIsExactReadOnly(t *testing.T) {
	if !billingAdminPathAllowed(http.MethodGet, "/admin/v1/finance-overviews/pricing") {
		t.Fatal("pricing read is unavailable")
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if billingAdminPathAllowed(method, "/admin/v1/finance-overviews/pricing") {
			t.Fatal("pricing overview mutation forwarded")
		}
	}
	for _, path := range []string{"/admin/v1/finance-overviews/pricing/secret", "/admin/v1/finance-overviews/other", "/admin/v2/finance-overviews/pricing"} {
		if billingAdminPathAllowed(http.MethodGet, path) {
			t.Fatal("noncanonical pricing overview forwarded")
		}
	}
}
