package token

import (
	"context"
	"encoding/json"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/portal"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

type retainedF04SignedPortalStore struct {
	portal.Store
	session portal.Session
}

func (s retainedF04SignedPortalStore) GetSession(_ context.Context, id string) (portal.Session, error) {
	if id != s.session.ID {
		return portal.Session{}, portal.ErrSessionNotFound
	}
	return s.session, nil
}

func TestRetainedF05PortalCurrentTokenSourceProtocol(t *testing.T) {
	for _, tc := range []struct {
		name, path, remote, source, alter string
		trusted                           bool
		want                              int
		canonical                         string
	}{
		{"operations-ipv4", "/admin/v1/overviews/model-operations?window=24h", "198.51.100.4:3210", "203.0.113.99", "", false, 200, "198.51.100.4"},
		{"operations-ipv6", "/admin/v1/overviews/model-operations?window=24h", "[2001:0db8:0000:0000:0000:0000:0000:0004]:3210", "203.0.113.99", "", false, 200, "2001:db8::4"},
		{"dual-window-query", "/admin/v1/overviews/model-operations?window=24h", "198.51.100.4:3210", "203.0.113.99", "", false, 200, "198.51.100.4"},
		{"trusted-edge-canonical-ipv6", "/admin/v1/overviews/model-operations?window=24h", "10.0.0.8:3210", "2001:0db8:0000:0000:0000:0000:0000:0005", "", true, 200, "2001:db8::5"},
		{"stripped-signature", "/admin/v1/overviews/model-operations?window=24h", "198.51.100.4:3210", "", "strip", false, 503, ""},
		{"changed-query", "/admin/v1/overviews/model-operations?window=24h", "198.51.100.4:3210", "", "query", false, 503, ""},
		{"foreign-signature", "/admin/v1/overviews/model-operations?window=24h", "198.51.100.4:3210", "", "signature", false, 503, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Authorization-Version", "7")
				_ = json.NewEncoder(w).Encode(map[string]any{"allowed": true, "granted_permissions": []string{"platform.models.read", "platform.tokens.read", "platform.usage.read"}})
			}))
			defer auth.Close()
			a, err := authorization.NewClient(auth.URL, "controlled-workload-token")
			if err != nil {
				t.Fatal(err)
			}
			stub := &modelTokenPageOverviewStub{values: validUsageOverviewValues(24 * time.Hour)}
			tok := pageOverviewService(stub, true).WithAuthorizer(a)
			subject := uuid.NewString()
			handler := tok.ManagementHandler("", func(context.Context, string) (identity.Principal, error) {
				return identity.Principal{Issuer: "controlled", Subject: subject, SessionID: "session"}, nil
			})
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer controlled-access" || r.Header.Get("Cookie") != "" {
					t.Error("Portal session authority changed")
				}
				switch tc.alter {
				case "strip":
					r.Header.Del("X-Pinvou-Client-Source-Signature")
				case "query":
					r.URL.RawQuery = "window=7d"
				case "signature":
					r.Header.Set("X-Pinvou-Client-Source-Signature", strings.Repeat("00", 32))
				}
				handler.ServeHTTP(w, r)
			}))
			defer backend.Close()
			parse := func(raw string) *url.URL {
				u, err := url.Parse(raw)
				if err != nil {
					t.Fatal(err)
				}
				return u
			}
			cfg := portal.Config{Environment: "test", ClientID: "portal", IssuerURL: parse("https://identity.example"), JWKSURL: parse("https://identity.example/keys"), PublicURL: parse("https://portal.example"), AuthorizationURL: parse("https://identity.example/auth"), TokenURL: parse("https://identity.example/token"), AccountURL: parse("https://account.example"), BillingURL: parse("https://billing.example"), TokenGatewayURL: parse(backend.URL), SecureCookies: true, AdminModelTokenPageOverviewsEnabled: true}
			// Config JSON preserves the before-build fixture: old Config ignores only the
			// missing field; after Config consumes the same synthetic protocol key.
			raw, _ := json.Marshal(map[string]any{"TokenAdminProxyHMACKey": pageOverviewProxyKey})
			if err = json.Unmarshal(raw, &cfg); err != nil {
				t.Fatal(err)
			}
			if tc.trusted {
				cfg.TrustedProxyIPs = []netip.Addr{netip.MustParseAddr("10.0.0.8")}
			}
			session := portal.Session{ID: "controlled-session", AccessToken: "controlled-access", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour)}
			svc, err := portal.NewService(cfg, retainedF04SignedPortalStore{session: session})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, "https://portal.example/api/admin/token"+tc.path, nil)
			req.RemoteAddr = tc.remote
			req.AddCookie(&http.Cookie{Name: "__Host-portal_session", Value: session.ID})
			req.Header.Set("X-Pinvou-Client-Source", tc.source)
			req.Header.Set("X-Pinvou-Client-Source-Signature", strings.Repeat("11", 32))
			req.Header.Set("X-Forwarded-For", "203.0.113.199")
			response := httptest.NewRecorder()
			svc.Handler(nil).ServeHTTP(response, req)
			if response.Code != tc.want {
				t.Fatalf("status=%d want=%d ownerCalls=%d body=%s", response.Code, tc.want, stub.queryCalls, response.Body.String())
			}
			if tc.want == 200 {
				if stub.queryCalls != 1 || stub.rateCalls != 1 || stub.source != tc.canonical || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("ownerCalls=%d rateCalls=%d source=%q", stub.queryCalls, stub.rateCalls, stub.source)
				}
			} else if stub.queryCalls != 0 || stub.rateCalls != 0 {
				t.Fatal("invalid signature reached owner")
			}
		})
	}
}
