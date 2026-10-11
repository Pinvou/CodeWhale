package identityadapter

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTokenProxyImmediatePeerDefault(t *testing.T) {
	for _, item := range []struct{ name, peer, forwarded, want string }{
		{"direct", "192.0.2.20:4000", "", "192.0.2.20"},
		{"forged single", "192.0.2.20:4000", "198.51.100.42", "192.0.2.20"},
		{"forged chain", "192.0.2.20:4000", "203.0.113.9, 198.51.100.42", "192.0.2.20"},
		{"IPv6 canonical", "[2001:db8:0:0::20]:4000", "2001:db8::42", "2001:db8::20"},
		{"mapped IPv4", "[::ffff:192.0.2.20]:4000", "198.51.100.42", "192.0.2.20"},
	} {
		t.Run(item.name, func(t *testing.T) {
			admission := &admissionFake{}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Forwarded-For") != item.want || r.Header.Get("X-Pinvou-Client-Source") != item.want {
					t.Error("upstream source differs from canonical immediate peer")
				}
				if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Real-IP") != "" || r.Header.Get("Forwarded") != "" {
					t.Error("untrusted caller headers were forwarded")
				}
				_, _ = w.Write([]byte(`{"access_token":"controlled"}`))
			}))
			defer upstream.Close()
			proxy, err := NewTokenProxy(upstream.URL, admission)
			if err != nil {
				t.Fatal(err)
			}
			proxy.floor = 0
			request := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader("grant_type=password&username=alice&password=controlled"))
			request.RemoteAddr = item.peer
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.Header.Set("X-Forwarded-For", item.forwarded)
			request.Header.Set("X-Pinvou-Client-Source", "203.0.113.200")
			request.Header.Set("X-Real-IP", "203.0.113.200")
			request.Header.Set("Forwarded", "for=203.0.113.200")
			request.Header.Set("Authorization", "Bearer controlled")
			request.Header.Set("Cookie", "controlled=value")
			response := httptest.NewRecorder()
			proxy.ServeHTTP(response, request)
			if response.Code != http.StatusOK || admission.dimensions.source != item.want {
				t.Fatalf("status=%d source=%q want=%q", response.Code, admission.dimensions.source, item.want)
			}
		})
	}
}

func TestTokenProxyExactTrustedPeer(t *testing.T) {
	for _, item := range []struct {
		name, peer string
		forwarded  []string
		want       string
	}{
		{"trusted IPv4", "192.0.2.20:4000", []string{"198.51.100.42"}, "198.51.100.42"},
		{"trusted IPv6 canonical", "[2001:db8:0:0::20]:4000", []string{"2001:db8:0:0::42"}, "2001:db8::42"},
		{"mapped trusted peer", "[::ffff:192.0.2.20]:4000", []string{"::ffff:198.51.100.42"}, "198.51.100.42"},
		{"different peer", "192.0.2.21:4000", []string{"198.51.100.42"}, "192.0.2.21"},
		{"missing header", "192.0.2.20:4000", nil, "192.0.2.20"},
		{"empty header", "192.0.2.20:4000", []string{""}, "192.0.2.20"},
		{"chain", "192.0.2.20:4000", []string{"203.0.113.9, 198.51.100.42"}, "192.0.2.20"},
		{"duplicate header", "192.0.2.20:4000", []string{"198.51.100.42", "198.51.100.42"}, "192.0.2.20"},
		{"invalid header", "192.0.2.20:4000", []string{"not-an-address"}, "192.0.2.20"},
		{"zoned header", "192.0.2.20:4000", []string{"fe80::42%edge"}, "192.0.2.20"},
	} {
		t.Run(item.name, func(t *testing.T) {
			admission := &admissionFake{}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Forwarded-For") != item.want || r.Header.Get("X-Pinvou-Client-Source") != item.want {
					t.Error("upstream source did not match admitted canonical source")
				}
				if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Real-IP") != "" || r.Header.Get("Forwarded") != "" {
					t.Error("browser headers forwarded")
				}
				_, _ = w.Write([]byte(`{"access_token":"controlled"}`))
			}))
			defer upstream.Close()
			proxy, err := NewTokenProxyWithTrustedPeers(upstream.URL, admission, "", false, "192.0.2.20,2001:db8::20")
			if err != nil {
				t.Fatal(err)
			}
			proxy.floor = 0
			request := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader("grant_type=password&username=alice&password=controlled"))
			request.RemoteAddr = item.peer
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			for _, value := range item.forwarded {
				request.Header.Add("X-Forwarded-For", value)
			}
			request.Header.Set("X-Pinvou-Client-Source", "203.0.113.200")
			request.Header.Set("X-Real-IP", "203.0.113.200")
			request.Header.Set("Forwarded", "for=203.0.113.200")
			request.Header.Set("Authorization", "Bearer controlled")
			request.Header.Set("Cookie", "controlled=value")
			response := httptest.NewRecorder()
			proxy.ServeHTTP(response, request)
			if response.Code != http.StatusOK || admission.dimensions.source != item.want {
				t.Fatalf("status=%d source=%q want=%q", response.Code, admission.dimensions.source, item.want)
			}
		})
	}
}

func TestTokenProxyInvalidPeerDoesNotAdmitOrForward(t *testing.T) {
	for _, peer := range []string{"", "192.0.2.20", "edge.example:4000", "[fe80::20%edge]:4000"} {
		t.Run(peer, func(t *testing.T) {
			admission := &admissionFake{}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("invalid peer reached upstream") }))
			defer upstream.Close()
			proxy, err := NewTokenProxyWithTrustedPeers(upstream.URL, admission, "", false, "192.0.2.20")
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader("grant_type=password&username=alice&password=controlled"))
			request.RemoteAddr = peer
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.Header.Set("X-Forwarded-For", "198.51.100.42")
			response := httptest.NewRecorder()
			proxy.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || admission.dimensions.action != "" || admission.completions != 0 || !strings.Contains(response.Body.String(), `"invalid_request"`) {
				t.Fatalf("invalid peer admitted: status=%d dimensions=%+v", response.Code, admission.dimensions)
			}
		})
	}
}

func TestTokenProxyTrustedPeerConfiguration(t *testing.T) {
	for _, item := range []struct {
		raw   string
		valid bool
		count int
	}{
		{"", true, 0}, {"  ", true, 0}, {"192.0.2.20,2001:db8::20", true, 2},
		{"::ffff:192.0.2.20,192.0.2.20", true, 1},
		{"192.0.2.0/24", false, 0}, {"edge.example", false, 0}, {"*", false, 0},
		{"192.0.2.20,", false, 0}, {",192.0.2.20", false, 0}, {"fe80::20%edge", false, 0},
	} {
		t.Run(item.raw, func(t *testing.T) {
			proxy, err := NewTokenProxyWithTrustedPeers("http://127.0.0.1/token", &admissionFake{}, "", false, item.raw)
			if item.valid {
				if err != nil || len(proxy.trustedProxyIPs) != item.count {
					t.Fatalf("valid configuration rejected: %v", err)
				}
			} else if err == nil || proxy != nil || err.Error() != "IDENTITY_TRUSTED_PROXY_IPS must contain only exact IP addresses without zones" {
				t.Fatalf("invalid configuration accepted or input disclosed: %v", err)
			}
		})
	}
}
