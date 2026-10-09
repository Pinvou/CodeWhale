package portal

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestPortalTrustedProxyConfigurationRejectsBroadOrInvalidTrust(t *testing.T) {
	for _, raw := range []string{"*", "0.0.0.0/0", "192.0.2.0/24", "edge.example", "192.0.2.45:443", "[2001:db8::1]", "fe80::1%eth0", ",", "192.0.2.45,", ",192.0.2.45", "192.0.2.45;2001:db8::1", "999.0.0.1"} {
		t.Run(raw, func(t *testing.T) {
			if _, err := ParseTrustedProxyIPs(raw); err == nil {
				t.Fatal("broad or invalid proxy configuration accepted")
			}
		})
	}
	for _, raw := range []string{"", " \t "} {
		addresses, err := ParseTrustedProxyIPs(raw)
		if err != nil || len(addresses) != 0 {
			t.Fatal("default must not trust any proxy")
		}
	}
	addresses, err := ParseTrustedProxyIPs("192.0.2.45, 2001:db8::45, ::ffff:192.0.2.45")
	if err != nil || len(addresses) != 3 || addresses[0] != addresses[2] {
		t.Fatal("exact IPv4/IPv6 normalization failed")
	}
	service := testService(t, newMemoryStore())
	for _, address := range []netip.Addr{{}, netip.MustParseAddr("fe80::1").WithZone("eth0")} {
		cfg := service.config
		cfg.TrustedProxyIPs = []netip.Addr{address}
		if _, err := NewService(cfg, newMemoryStore()); err == nil {
			t.Fatal("typed invalid proxy configuration bypassed constructor")
		}
	}
}

func TestPortalTrustedProxyRiskSourceUsesExactImmediatePeer(t *testing.T) {
	for _, test := range []struct {
		name, configured, remote, want string
		header                         []string
		status                         int
	}{
		{"default-spoof", "", "192.0.2.45:443", "192.0.2.45", []string{"203.0.113.99"}, 200},
		{"default-private-peer", "", "10.0.0.2:443", "10.0.0.2", []string{"203.0.113.99"}, 200},
		{"trusted", "192.0.2.45", "192.0.2.45:443", "203.0.113.99", []string{"203.0.113.99"}, 200},
		{"untrusted", "192.0.2.45", "198.51.100.45:443", "198.51.100.45", []string{"203.0.113.99"}, 200},
		{"trusted-missing", "192.0.2.45", "192.0.2.45:443", "192.0.2.45", nil, 200},
		{"trusted-empty", "192.0.2.45", "192.0.2.45:443", "192.0.2.45", []string{""}, 200},
		{"trusted-list", "192.0.2.45", "192.0.2.45:443", "192.0.2.45", []string{"203.0.113.99, 203.0.113.98"}, 200},
		{"trusted-multiple", "192.0.2.45", "192.0.2.45:443", "192.0.2.45", []string{"203.0.113.99", "203.0.113.99"}, 200},
		{"trusted-hostname", "192.0.2.45", "192.0.2.45:443", "192.0.2.45", []string{"client.example"}, 200},
		{"trusted-port", "192.0.2.45", "192.0.2.45:443", "192.0.2.45", []string{"203.0.113.99:80"}, 200},
		{"trusted-zone", "192.0.2.45", "192.0.2.45:443", "192.0.2.45", []string{"fe80::1%eth0"}, 200},
		{"trusted-ipv6", "2001:db8::45", "[2001:db8::45]:443", "2001:db8::99", []string{"2001:db8::99"}, 200},
		{"untrusted-ipv6", "2001:db8::45", "[2001:db8::46]:443", "2001:db8::46", []string{"2001:db8::99"}, 200},
		{"mapped-peer", "192.0.2.45", "[::ffff:192.0.2.45]:443", "203.0.113.99", []string{"::ffff:203.0.113.99"}, 200},
		{"invalid-peer", "192.0.2.45", "edge.example:443", "", []string{"203.0.113.99"}, 400},
		{"peer-missing-port", "192.0.2.45", "192.0.2.45", "", []string{"203.0.113.99"}, 400},
	} {
		for _, route := range []string{"/v1/auth/sms/challenges", "/v1/auth/registration/precheck", "/v1/auth/captcha/challenges"} {
			t.Run(test.name+route, func(t *testing.T) {
				observed := make(chan string, 1)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Real-IP") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Internal-Service-Token") != "" {
						t.Error("untrusted authority/proxy headers reached Account")
					}
					if route != "/v1/auth/sms/challenges" && r.Header.Get("X-Device-ID") != "" {
						t.Error("public admission gained device proof authority")
					}
					observed <- r.Header.Get("X-Pinvou-Client-Source")
					w.WriteHeader(200)
				}))
				defer upstream.Close()
				original := testService(t, newMemoryStore())
				cfg := original.config
				var err error
				cfg.TrustedProxyIPs, err = ParseTrustedProxyIPs(test.configured)
				if err != nil {
					t.Fatal(err)
				}
				cfg.AccountURL = parseTestURL(t, upstream.URL)
				service, err := NewService(cfg, newMemoryStore())
				if err != nil {
					t.Fatal(err)
				}
				// Caller mutations after construction cannot change the trusted identity set.
				if len(cfg.TrustedProxyIPs) > 0 {
					cfg.TrustedProxyIPs[0] = netip.MustParseAddr("203.0.113.1")
				}
				request := httptest.NewRequest("POST", "/api/account"+route, strings.NewReader(`{}`))
				request.RemoteAddr = test.remote
				request.Header.Set("Origin", service.config.PublicURL.String())
				request.Header["X-Pinvou-Client-Source"] = test.header
				request.Header.Set("X-Forwarded-For", "203.0.113.98")
				request.Header.Set("X-Real-IP", "203.0.113.97")
				request.Header.Set("Cookie", "browser-cookie=controlled")
				request.Header.Set("Authorization", "Bearer controlled-browser")
				request.Header.Set("X-Internal-Service-Token", "controlled-browser")
				request.Header.Set("X-Device-ID", "controlled-browser-device")
				response := httptest.NewRecorder()
				service.Handler(nil).ServeHTTP(response, request)
				if response.Code != test.status {
					t.Fatalf("status=%d", response.Code)
				}
				select {
				case got := <-observed:
					if test.status != 200 || got != test.want {
						t.Fatal("risk source crossed the configured peer boundary")
					}
				default:
					if test.status == 200 {
						t.Fatal("permitted risk request was not forwarded")
					}
				}
			})
		}
	}
}

func TestPortalTrustedProxyHeaderRemainsRiskRouteBound(t *testing.T) {
	for _, test := range []struct{ method, route string }{
		{http.MethodGet, "/v1/auth/registration/precheck"},
		{http.MethodGet, "/v1/auth/captcha/challenges"},
		{http.MethodPost, "/v1/auth/login"},
		{http.MethodPost, "/v1/auth/captcha/challenges/extra"},
		{http.MethodGet, "/v1/auth/registration-operations/11111111-1111-4111-8111-111111111111"},
	} {
		if accountRiskSourceAllowed(test.method, test.route) {
			t.Fatal("risk source forwarding escaped its exact operation allowlist")
		}
	}
	observed := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed <- r.Header.Get("X-Pinvou-Client-Source")
		w.WriteHeader(200)
	}))
	defer upstream.Close()
	original := testService(t, newMemoryStore())
	cfg := original.config
	cfg.TrustedProxyIPs = []netip.Addr{netip.MustParseAddr("192.0.2.45")}
	cfg.AccountURL = parseTestURL(t, upstream.URL)
	service, err := NewService(cfg, newMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"/v1/auth/registration/precheck", "/v1/auth/captcha/challenges"} {
		request := httptest.NewRequest("POST", "/api/account"+route, strings.NewReader(`{}`))
		request.RemoteAddr = "192.0.2.45:443"
		request.Header.Set("Origin", service.config.PublicURL.String())
		request.Header.Set("X-Pinvou-Client-Source", "203.0.113.99")
		response := httptest.NewRecorder()
		service.Handler(nil).ServeHTTP(response, request)
		if response.Code != 200 || <-observed != "203.0.113.99" {
			t.Fatal("public admission lost its trusted client source")
		}
		if accountProofHeaderAllowed(http.MethodPost, route, "X-Device-ID") {
			t.Fatal("risk attribution expanded device proof authority")
		}
	}
}
