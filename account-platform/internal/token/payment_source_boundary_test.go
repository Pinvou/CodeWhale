package token

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Expectations describe transport authority, not the implementation's choice
// of header precedence. Bare forwarding headers are never authority.
func TestPaymentSourceBoundary(t *testing.T) {
	key := []byte("controlled-payment-source-key-32bytes")
	for _, tc := range []struct {
		name, remote, source, change, want string
		signed, configured                 bool
	}{
		{"direct-forged", "198.51.100.15:40100", "203.0.113.20", "", "198.51.100.15", false, true},
		{"private-unsigned", "10.0.0.4:40100", "203.0.113.20", "", "10.0.0.4", false, true},
		{"signed-private", "10.0.0.4:40100", "203.0.113.20", "", "203.0.113.20", true, true},
		{"signed-loopback-ipv6", "[::1]:40100", "2001:db8::20", "", "2001:db8::20", true, true},
		{"signed-public-untrusted-peer", "198.51.100.15:40100", "203.0.113.20", "", "198.51.100.15", true, true},
		{"changed-uri", "10.0.0.4:40100", "203.0.113.20", "uri", "10.0.0.4", true, true},
		{"changed-method", "10.0.0.4:40100", "203.0.113.20", "method", "10.0.0.4", true, true},
		{"changed-signature", "10.0.0.4:40100", "203.0.113.20", "signature", "10.0.0.4", true, true},
		{"missing-key", "10.0.0.4:40100", "203.0.113.20", "", "10.0.0.4", true, false},
		{"direct-ipv6", "[2001:0db8:0:0:0:0:0:15]:40100", "203.0.113.20", "", "2001:db8::15", false, true},
		{"unknown-peer", "invalid-peer", "203.0.113.20", "", "source:unknown", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{}
			if tc.configured {
				s.WithAdminProxyHMACKey(key)
			}
			r := httptest.NewRequest(http.MethodGet, "/v1/recharge/orders/11111111-1111-4111-8111-111111111111?original=1", nil)
			r.RemoteAddr = tc.remote
			r.Header.Set("X-Forwarded-For", "203.0.113.199, 203.0.113.198")
			r.Header.Set("X-Pinvou-Client-Source", tc.source)
			if tc.signed {
				signAdminQuerySource(r, key, tc.source)
			}
			switch tc.change {
			case "uri":
				r.URL.RawQuery = "original=2"
			case "method":
				r.Method = http.MethodPost
			case "signature":
				r.Header.Set("X-Pinvou-Client-Source-Signature", strings.Repeat("00", 32))
			}
			if got := s.paymentSourceContext(r).Value(paymentSourceContextKey{}); got != tc.want {
				t.Fatalf("source=%v, want transport-authoritative %s", got, tc.want)
			}
		})
	}
}
