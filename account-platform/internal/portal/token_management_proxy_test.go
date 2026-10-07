package portal

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

func TestF04ManagementProxyPreservesFrozenFactsAndServerSession(t *testing.T) {
	for _, serviceName := range []string{"token", "billing"} {
		t.Run(serviceName, func(t *testing.T) {
			store := newMemoryStore()
			service := testService(t, store)
			session := validSession(uuid.NewString())
			if err := store.SaveSession(context.Background(), session); err != nil {
				t.Fatal(err)
			}
			path := "/admin/v1/commands"
			if serviceName == "billing" {
				path = "/admin/v1/pricing/drafts"
			}
			body := `{"expected_etag":9007199254740993,"reason":"controlled management request"}`
			receipt := `{"space_etag":"9007199254740994"}`
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				raw, err := io.ReadAll(r.Body)
				if err != nil || string(raw) != body || r.URL.Path != path || r.URL.Query().Get("controlled") != "1" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+session.AccessToken || r.Header.Get("Idempotency-Key") != "controlled-original-invocation" || r.Header.Get("X-CSRF-Token") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Pinvou-Approval-Proof-Test") != "" {
					t.Error("management facts or server authority changed")
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "3")
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, receipt)
			}))
			defer upstream.Close()
			if serviceName == "token" {
				service.config.TokenGatewayURL = parseTestURL(t, upstream.URL)
			} else {
				service.config.BillingURL = parseTestURL(t, upstream.URL)
			}
			target := "/api/admin/" + serviceName + path + "?controlled=1"
			request := financeBrowserRequest(service, session, http.MethodPost, target, body)
			request.Header.Set("Authorization", "Bearer browser-untrusted")
			request.Header.Set("Idempotency-Key", "controlled-original-invocation")
			request.Header.Set("X-Pinvou-Approval-Proof-Test", "untrusted")
			response := httptest.NewRecorder()
			service.Handler(http.NotFoundHandler()).ServeHTTP(response, request)
			if response.Code != http.StatusCreated || response.Body.String() != receipt || response.Header().Get("Retry-After") != "3" || response.Header().Get("Cache-Control") != "no-store" || calls.Load() != 1 {
				t.Fatal("management receipt was not preserved")
			}
			for _, attack := range []string{"csrf", "origin", "session"} {
				rejected := financeBrowserRequest(service, session, http.MethodPost, target, body)
				switch attack {
				case "csrf":
					rejected.Header.Set("X-CSRF-Token", "wrong")
				case "origin":
					rejected.Header.Set("Origin", "https://untrusted.example")
				case "session":
					rejected.Header.Set("Cookie", "")
				}
				response = httptest.NewRecorder()
				service.Handler(nil).ServeHTTP(response, rejected)
				if response.Code != http.StatusForbidden && response.Code != http.StatusUnauthorized {
					t.Fatalf("%s status=%d", attack, response.Code)
				}
				if calls.Load() != 1 {
					t.Fatalf("%s reached upstream", attack)
				}
			}
		})
	}
}

func TestF04ManagementProxyRejectsFutureAndMalformedOperations(t *testing.T) {
	service := testService(t, newMemoryStore())
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer upstream.Close()
	service.config.TokenGatewayURL = parseTestURL(t, upstream.URL)
	service.config.BillingURL = parseTestURL(t, upstream.URL)
	id := uuid.NewString()
	for _, target := range []struct{ method, path string }{
		{"POST", "/api/admin/token/admin/v1/token-accounts"}, {"DELETE", "/api/admin/token/admin/v1/token-accounts/" + id},
		{"GET", "/api/admin/token/admin/v1/usage-requests"}, {"GET", "/api/admin/token/admin/v1/model-operations"},
		{"POST", "/api/admin/token/v1/chat/completions"}, {"POST", "/api/admin/token/v1/recharge/orders"},
		{"GET", "/api/admin/token/admin/v1/models/not-a-uuid"}, {"GET", "/api/admin/token/admin/v1/models/00000000-0000-0000-0000-000000000000"},
		{"POST", "/api/admin/billing/admin/v1/pricing/" + id + "/delete"}, {"PUT", "/api/admin/billing/admin/v1/metering-units/" + id},
		{"POST", "/api/admin/billing/admin/v1/pricing/not-a-uuid/publish"}, {"GET", "/api/admin/billing/admin/v1/usage-holds"},
	} {
		t.Run(target.method+target.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(response, httptest.NewRequest(target.method, target.path, nil))
			if response.Code != http.StatusNotFound || calls.Load() != 0 {
				t.Fatalf("unsupported route status=%d calls=%d", response.Code, calls.Load())
			}
		})
	}
}

func TestF04ManagementInstalledRouteAllowlist(t *testing.T) {
	id := uuid.NewString()
	for _, route := range []struct{ method, path string }{
		{"GET", "/admin/v1/models"}, {"GET", "/admin/v1/models/" + id}, {"GET", "/admin/v1/models/" + id + "/versions"},
		{"POST", "/admin/v1/models/" + id + "/lifecycle-plans"}, {"POST", "/admin/v1/models/" + id + "/lifecycle-plans/" + id + "/cancel"},
		{"GET", "/admin/v1/models/" + id + "/impact/lifecycle"}, {"GET", "/admin/v1/model-catalog/impact/rollback"},
		{"GET", "/admin/v1/protocols"}, {"GET", "/admin/v1/formulas"}, {"GET", "/admin/v1/formulas/" + id + "/preview"},
		{"GET", "/admin/v1/token-resources/readiness"}, {"GET", "/admin/v1/owner-candidates"}, {"GET", "/admin/v1/resource-audit"},
		{"GET", "/admin/v1/token-accounts/" + id + "/credentials"}, {"GET", "/admin/v1/token-accounts/" + id + "/routes"},
		{"POST", "/admin/v1/token-accounts/" + id + "/credential-launches/enroll"}, {"POST", "/admin/v1/token-accounts/" + id + "/credential-launches/rotate"},
		{"GET", "/admin/v1/credential-results/" + id}, {"POST", "/admin/v1/credential-results/" + id + "/revoke-window"},
		{"GET", "/admin/v1/provisionings"}, {"GET", "/admin/v1/provisionings/" + id + "/attempts"},
		{"POST", "/admin/v1/commands:precheck"}, {"POST", "/admin/v1/commands"}, {"GET", "/admin/v1/commands/" + id},
		{"POST", "/admin/v1/commands/" + id + "/recovery-actions"},
	} {
		if !tokenAdminPathAllowed(route.method, route.path) {
			t.Errorf("installed Token operation rejected: %s %s", route.method, route.path)
		}
	}
	for _, route := range []struct{ method, path string }{
		{"GET", "/admin/v1/pricing"}, {"POST", "/admin/v1/pricing/drafts"}, {"PUT", "/admin/v1/pricing/" + id + "/draft"},
		{"POST", "/admin/v1/pricing/" + id + "/publish"}, {"POST", "/admin/v1/pricing/" + id + "/transition"}, {"GET", "/admin/v1/pricing/" + id + "/preview"},
		{"GET", "/admin/v1/metering-units"}, {"POST", "/admin/v1/metering-units"}, {"POST", "/admin/v1/metering-units/" + id + "/deactivate"},
		{"GET", "/admin/v1/models/" + id + "/price-projection"},
	} {
		if !billingAdminPathAllowed(route.method, route.path) {
			t.Errorf("installed Billing operation rejected: %s %s", route.method, route.path)
		}
	}
}

func TestF04CredentialOriginSessionProjection(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	session := validSession(uuid.NewString())
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	service.config.AdminTokenDetailsEnabled = true
	service.config.CredentialOrigin = parseTestURL(t, "https://credentials-uat.pinvou.com")
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, financeBrowserRequest(service, session, http.MethodGet, "/api/session", ""))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"credential_origin":"https://credentials-uat.pinvou.com"`) || strings.Contains(response.Body.String(), session.AccessToken) {
		t.Fatal("authenticated session did not project isolated origin safely")
	}
}

func TestF04CredentialOriginIsolationAndHead(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	config := service.config
	config.AdminTokenDetailsEnabled = true
	config.CredentialOrigin = parseTestURL(t, "https://credentials-uat.pinvou.com")
	config.PublicURL = config.CredentialOrigin
	if _, err := NewService(config, store); err == nil {
		t.Fatal("management and credential origins may not share a browser origin")
	}
	config.PublicURL = service.config.PublicURL
	config.CredentialOrigin = parseTestURL(t, "https://credentials-uat.pinvou.com?")
	if _, err := NewService(config, store); err == nil {
		t.Fatal("credential origin with a forced query was accepted")
	}
	service.config.AdminTokenDetailsEnabled = true
	service.config.CredentialOrigin = parseTestURL(t, "https://CREDENTIALS-UAT.PINVOU.COM")
	response := httptest.NewRecorder()
	service.Handler(http.NotFoundHandler()).ServeHTTP(response, httptest.NewRequest(http.MethodHead, "/credential-handoff", nil))
	if response.Code != http.StatusOK || response.Body.Len() != 0 || !strings.Contains(response.Header().Get("Content-Security-Policy"), "form-action https://credentials-uat.pinvou.com") {
		t.Fatal("credential HEAD was not the same fixed isolated document contract")
	}
}

func TestF04GovernedApprovalCommandBoundary(t *testing.T) {
	for _, command := range []string{"WALLET_ADJUSTMENT", "PAYMENT_PROVIDER_RESUME", "PAYMENT_PRODUCT_RESUME", "TOKEN_MODEL_LIFECYCLE_CHANGE", "TOKEN_MODEL_CATALOG_ROLLBACK", "TOKEN_CREDENTIAL_REVOKE"} {
		if !currentGovernedApprovalCommand(command) {
			t.Errorf("current command rejected: %s", command)
		}
	}
	for _, command := range []string{"", "USAGE_REPAIR", "TOKEN_ACCOUNT_CREATE", "TOKEN_ORG_QUOTA_ALLOCATE", "ACCOUNT_DELETE", "TOKEN_MODEL_LIFECYCLE_CHANGE "} {
		if currentGovernedApprovalCommand(command) {
			t.Errorf("foreign command admitted: %s", command)
		}
	}
}
