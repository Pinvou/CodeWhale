package identityadapter

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTokenProxyPreservesAdmittedSourceForKeycloakTrustedProxy(t *testing.T) {
	for _, test := range []struct {
		name, forwarded, remote, want string
	}{
		{"edge IPv4", "198.51.100.42", "192.0.2.20:43000", "198.51.100.42"},
		{"edge IPv6", "2001:db8::42", "[2001:db8::20]:43000", "2001:db8::42"},
		{"forwarded chain falls back to peer", "203.0.113.9, 198.51.100.42", "192.0.2.20:43000", "192.0.2.20"},
		{"direct peer", "", "192.0.2.20:43000", "192.0.2.20"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var forwarded, source, authorization, cookie string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				forwarded = r.Header.Get("X-Forwarded-For")
				source = r.Header.Get("X-Pinvou-Client-Source")
				authorization = r.Header.Get("Authorization")
				cookie = r.Header.Get("Cookie")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"access_token":"controlled"}`))
			}))
			defer upstream.Close()
			admission := &admissionFake{}
			proxy, err := NewTokenProxyWithTrustedPeers(upstream.URL, admission, "", false, "192.0.2.20,2001:db8::20")
			if err != nil {
				t.Fatal(err)
			}
			proxy.floor = 0
			request := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader("grant_type=password&username=alice&password=controlled"))
			request.RemoteAddr = test.remote
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.Header.Set("X-Forwarded-For", test.forwarded)
			request.Header.Set("X-Pinvou-Client-Source", "203.0.113.200")
			request.Header.Set("Authorization", "Bearer browser-value")
			request.Header.Set("Cookie", "browser=value")
			response := httptest.NewRecorder()
			proxy.ServeHTTP(response, request)
			if response.Code != http.StatusOK || admission.dimensions.source != test.want || forwarded != test.want || source != test.want || authorization != "" || cookie != "" {
				t.Fatalf("status=%d admitted=%q forwarded=%q source=%q browserHeadersForwarded=%v", response.Code, admission.dimensions.source, forwarded, source, authorization != "" || cookie != "")
			}
		})
	}
}
