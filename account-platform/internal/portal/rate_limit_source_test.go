package portal

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

func TestPortalRateLimitSourceActualBFFHops(t *testing.T) {
	for _, route := range []string{"/api/account/v1/me", "/api/token/v1/me/balance", "/api/admin/billing/admin/v1/payment-orders"} {
		t.Run(route, func(t *testing.T) {
			store := newMemoryStore()
			s := testService(t, store)
			session := validSession("controlled-source-session")
			if err := store.SaveSession(context.Background(), session); err != nil {
				t.Fatal(err)
			}
			cfg := config.Config{ServiceName: "backend-fixture", Environment: "test", RateLimitRPS: 0.001, RateLimitBurst: 1, HTTPTrustedProxyIPs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}
			calls := 0
			backend := httptest.NewServer(httpserver.HandlerWithApplication(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				source := r.Header.Get("X-Pinvou-RateLimit-Source")
				if source != "198.51.100.1" && source != "198.51.100.2" {
					t.Error("BFF did not replace admission source")
				}
				if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Real-IP") != "" || r.Header.Get("X-Pinvou-Client-Source") != "" || r.Header.Get("X-Pinvou-Client-Source-Signature") != "" {
					t.Error("unrelated business source/header authority widened")
				}
				if r.Header.Get("Authorization") != "Bearer "+session.AccessToken {
					t.Error("existing session authority changed")
				}
				w.WriteHeader(200)
			}), nil))
			defer backend.Close()
			s.config.AccountURL = parseTestURL(t, backend.URL)
			s.config.TokenGatewayURL = parseTestURL(t, backend.URL)
			s.config.BillingURL = parseTestURL(t, backend.URL)
			// General ingress trust is distinct from path-specific business trust.
			cfg.ServiceName = "portal-fixture"
			cfg.HTTPTrustedProxyIPs = []netip.Addr{netip.MustParseAddr("192.0.2.1")}
			h := httpserver.HandlerWithApplication(cfg, s.Handler(nil), nil)
			for i, source := range []string{"198.51.100.1", "198.51.100.2", "198.51.100.1"} {
				r := httptest.NewRequest("GET", route, nil)
				r.RemoteAddr = "192.0.2.1:40000"
				r.Header.Set("X-Pinvou-RateLimit-Source", source)
				r.Header.Set("X-Pinvou-Client-Source", "203.0.113.201")
				r.Header.Set("X-Forwarded-For", "203.0.113.202")
				r.AddCookie(&http.Cookie{Name: s.sessionCookieName(), Value: session.ID})
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				want := 200
				if i == 2 {
					want = 429
				}
				if w.Code != want {
					t.Fatalf("same edge peer user %d=%d want%d", i, w.Code, want)
				}
			}
			if calls != 2 {
				t.Fatal("unexpected BFF backend count", calls)
			}
		})
	}
}

func TestPortalApprovalPaginationSourceHop(t *testing.T) {
	store := &financeWireStore{memoryStore: newMemoryStore(), wire: map[string]int{}}
	s := testService(t, store)
	session := validSession("approval-source-session")
	session.ApprovalV2Eligible = true
	if err := store.SaveSession(t.Context(), session); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var sources []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/admin/v2/approval-capability" {
			_, _ = io.WriteString(w, `{"schema_version":1,"mode":"PROOF_REQUIRED","approval_wire_version":2,"proof_required":true,"epoch":1,"authorization_relay_state":"RUNNING","authorization_relay_epoch":1,"version":1,"request_id":"controlled"}`)
			return
		}
		if r.URL.Path != "/admin/v2/approval-requests" || r.URL.RawQuery != "limit=17&status=APPROVED" || r.Header.Get("Authorization") != "Bearer "+session.AccessToken || r.Header.Get("Cookie") != "" || r.Header.Get("X-Forwarded-For") != "" {
			t.Error("approval routing, query or session authority changed")
		}
		mu.Lock()
		sources = append(sources, r.Header.Get(httpserver.RateLimitSourceHeader))
		mu.Unlock()
		_, _ = io.WriteString(w, `{"items":[],"limit":17,"has_more":false}`)
	}))
	defer backend.Close()
	s.config.AccountURL = parseTestURL(t, backend.URL)
	cfg := config.Config{ServiceName: "portal-source-test", Environment: "test", RateLimitRPS: 100, RateLimitBurst: 200, HTTPTrustedProxyIPs: []netip.Addr{netip.MustParseAddr("192.0.2.1")}}
	h := httpserver.HandlerWithApplication(cfg, s.Handler(nil), nil)
	for _, source := range []string{"198.51.100.11", "198.51.100.12"} {
		r := httptest.NewRequest(http.MethodGet, "/api/admin/approvals/requests?limit=17&status=APPROVED", nil)
		r.RemoteAddr = "192.0.2.1:1234"
		r.Header.Set(httpserver.RateLimitSourceHeader, source)
		r.Header.Set("X-Forwarded-For", "203.0.113.99")
		r.AddCookie(&http.Cookie{Name: s.sessionCookieName(), Value: session.ID})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("approval BFF status=%d body=%s", w.Code, w.Body.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sources) != 2 || sources[0] != "198.51.100.11" || sources[1] != "198.51.100.12" {
		t.Fatalf("approval BFF did not preserve admitted client separation: %v", sources)
	}
}
