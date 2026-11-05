package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
)

func TestHTTPTrustedRateLimitSource(t *testing.T) {
	for _, tc := range []struct {
		name, peers, peer, first, second string
		duplicate                        bool
		want                             int
	}{
		{"trusted-distinct", "192.0.2.1", "192.0.2.1:123", "198.51.100.1", "198.51.100.2", false, 204},
		{"no-trust", "", "192.0.2.1:123", "198.51.100.1", "198.51.100.2", false, 429},
		{"untrusted-spoof", "192.0.2.2", "192.0.2.1:123", "198.51.100.1", "198.51.100.2", false, 429},
		{"v4-mapped-alias", "192.0.2.1", "192.0.2.1:123", "198.51.100.1", "::ffff:198.51.100.1", false, 429},
		{"v6-alias", "2001:db8::1", "[2001:0db8::1]:123", "2001:db8::2", "2001:0db8:0:0::2", false, 429},
		{"empty-fallback", "192.0.2.1", "192.0.2.1:123", "", "", false, 429},
		{"list-fallback", "192.0.2.1", "192.0.2.1:123", "198.51.100.1, 198.51.100.2", "example.test", false, 429},
		{"zone-fallback", "192.0.2.1", "192.0.2.1:123", "fe80::1%eth0", "bad", false, 429},
		{"duplicate-fallback", "192.0.2.1", "192.0.2.1:123", "198.51.100.1", "198.51.100.2", true, 429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("APP_ENV", "development")
			t.Setenv("RATE_LIMIT_RPS", "0.001")
			t.Setenv("RATE_LIMIT_BURST", "1")
			t.Setenv("HTTP_TRUSTED_PROXY_IPS", tc.peers)
			cfg, err := config.Load("fixture")
			if err != nil {
				t.Fatal(err)
			}
			h := HandlerWithApplication(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }), nil)
			for i, source := range []string{tc.first, tc.second} {
				r := httptest.NewRequest("GET", "/fixture", nil)
				r.RemoteAddr = tc.peer
				r.Header.Set("X-Pinvou-RateLimit-Source", source)
				if tc.duplicate {
					r.Header.Add("X-Pinvou-RateLimit-Source", source)
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				want := 204
				if i == 1 {
					want = tc.want
				}
				if w.Code != want {
					t.Fatalf("attempt %d status=%d want=%d", i, w.Code, want)
				}
			}
			for _, path := range []string{"/health/live", "/health/ready"} {
				w := httptest.NewRecorder()
				r := httptest.NewRequest("GET", path, nil)
				r.RemoteAddr = tc.peer
				h.ServeHTTP(w, r)
				if w.Code != 200 {
					t.Fatalf("probe %s=%d", path, w.Code)
				}
			}
		})
	}
}
