package portal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
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
