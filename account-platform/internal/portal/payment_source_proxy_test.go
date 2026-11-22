package portal

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPortalPaymentSourceForwarding(t *testing.T) {
	org := "11111111-1111-4111-8111-111111111111"
	order := "22222222-2222-4222-8222-222222222222"
	for _, configured := range []bool{false, true} {
		for _, tc := range []struct {
			method, path string
			payment      bool
		}{
			{http.MethodPost, "/v1/recharge/orders", true},
			{http.MethodGet, "/v1/recharge/orders/" + order, true},
			{http.MethodHead, "/v1/recharge/orders/" + order, true},
			{http.MethodPost, "/v1/organizations/" + org + "/recharge/orders", true},
			{http.MethodGet, "/v1/organizations/" + org + "/recharge/orders/" + order, true},
			{http.MethodHead, "/v1/organizations/" + org + "/recharge/orders/" + order, true},
			{http.MethodGet, "/v1/me/balance", false},
			{http.MethodGet, "/v1/recharge/products", false},
			{http.MethodGet, "/v1/recharge/orders", false},
			{http.MethodGet, "/v1/recharge/orders/recent", false},
			{http.MethodPost, "/v1/recharge/orders/" + order + "/close", false},
			{http.MethodGet, "/v1/organizations/" + org + "/balance", false},
			{http.MethodGet, "/v1/organizations/" + org + "/recharge/products", false},
		} {
			name := "without-key"
			if configured {
				name = "signed"
			}
			t.Run(name+tc.method+tc.path, func(t *testing.T) {
				key := []byte("controlled-payment-source-key-32bytes")
				store := newMemoryStore()
				s := testService(t, store)
				if configured {
					s.config.TokenAdminProxyHMACKey = key
				}
				session := validSession("controlled-payment-session")
				if err := store.SaveSession(context.Background(), session); err != nil {
					t.Fatal(err)
				}
				calls := 0
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					if r.Method != tc.method || r.URL.Path != tc.path || r.URL.RawQuery != "original=1" || r.Header.Get("Authorization") != "Bearer "+session.AccessToken {
						t.Error("proxy changed the existing request authority or URI")
					}
					if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-CSRF-Token") != "" {
						t.Error("unsigned browser forwarding or session proof escaped the Portal")
					}
					if configured && tc.payment {
						if r.Header.Get("X-Pinvou-Client-Source") != "198.51.100.15" {
							t.Error("signed source is not the actual Portal client")
						}
						mac := hmac.New(sha256.New, key)
						_, _ = mac.Write([]byte(tc.method + "\n" + tc.path + "?original=1\n198.51.100.15"))
						signature, err := hex.DecodeString(r.Header.Get("X-Pinvou-Client-Source-Signature"))
						if err != nil || !hmac.Equal(signature, mac.Sum(nil)) {
							t.Error("payment source lost the existing method/URI-bound signature")
						}
					} else if r.Header.Get("X-Pinvou-Client-Source") != "" || r.Header.Get("X-Pinvou-Client-Source-Signature") != "" {
						t.Error("missing configuration or unrelated route inherited browser source authority")
					}
					w.WriteHeader(http.StatusOK)
				}))
				defer upstream.Close()
				s.config.TokenGatewayURL = parseTestURL(t, upstream.URL)
				r := httptest.NewRequest(tc.method, "https://portal.example/api/token"+tc.path+"?original=1", strings.NewReader(`{}`))
				r.RemoteAddr = "198.51.100.15:40100"
				r.Header.Set("Origin", "https://portal.example")
				r.Header.Set("X-CSRF-Token", session.CSRFToken)
				r.Header.Set("X-Forwarded-For", "203.0.113.200")
				r.Header.Set("X-Pinvou-Client-Source", "203.0.113.201")
				r.Header.Set("X-Pinvou-Client-Source-Signature", strings.Repeat("00", 32))
				r.AddCookie(&http.Cookie{Name: s.sessionCookieName(), Value: session.ID})
				r.AddCookie(&http.Cookie{Name: s.csrfCookieName(), Value: session.CSRFToken})
				out := httptest.NewRecorder()
				s.Handler(nil).ServeHTTP(out, r)
				if tc.payment && !configured {
					if out.Code != http.StatusServiceUnavailable || calls != 0 || tc.method != http.MethodHead && !strings.Contains(out.Body.String(), `"code":"PAYMENT_SOURCE_UNAVAILABLE"`) {
						t.Fatalf("unsigned payment was forwarded or not explicitly refused: status=%d calls=%d", out.Code, calls)
					}
				} else if out.Code != http.StatusOK || calls != 1 {
					t.Fatalf("status=%d calls=%d", out.Code, calls)
				}
			})
		}
	}
}
