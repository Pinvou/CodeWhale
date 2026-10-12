package account

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTrustedSourceBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, peers, remote, expected string
		headers                       []string
	}{
		{"default ignores forged header", "", "192.0.2.10:1234", "192.0.2.10", []string{"198.51.100.1"}},
		{"untrusted peer", "192.0.2.20", "192.0.2.10:1234", "192.0.2.10", []string{"198.51.100.1"}},
		{"trusted peer", "192.0.2.10", "192.0.2.10:1234", "198.51.100.1", []string{"198.51.100.1"}},
		{"multiple headers", "192.0.2.10", "192.0.2.10:1234", "192.0.2.10", []string{"198.51.100.1", "198.51.100.2"}},
		{"header list", "192.0.2.10", "192.0.2.10:1234", "192.0.2.10", []string{"198.51.100.1,198.51.100.2"}},
		{"invalid source", "192.0.2.10", "192.0.2.10:1234", "192.0.2.10", []string{"localhost"}},
		{"zone rejected", "192.0.2.10", "192.0.2.10:1234", "192.0.2.10", []string{"fe80::1%eth0"}},
		{"mapped peer and source", "192.0.2.10", "[::ffff:192.0.2.10]:1234", "198.51.100.1", []string{"::ffff:198.51.100.1"}},
		{"IPv6", "2001:db8::1", "[2001:db8::1]:1234", "2001:db8::2", []string{"2001:db8::2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := requestSource(r); got != tc.expected {
					t.Fatalf("source=%q want=%q", got, tc.expected)
				}
			})
			handler, err := NewTrustedSourceHandler(next, tc.peers)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("POST", "/v1/auth/sms/challenges", nil)
			r.RemoteAddr = tc.remote
			for _, value := range tc.headers {
				r.Header.Add("X-Pinvou-Client-Source", value)
			}
			handler.ServeHTTP(httptest.NewRecorder(), r)
		})
	}
}

func TestTrustedSourceConfigurationFailsClosed(t *testing.T) {
	for _, raw := range []string{"0.0.0.0/0", "192.0.2.1,", "localhost", "*", "fe80::1%eth0", "0.0.0.0", "224.0.0.1"} {
		if _, err := NewTrustedSourceHandler(http.NotFoundHandler(), raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}
