package portal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestPersonalOperationHeadersStayRouteBound(t *testing.T) {
	id := "11111111-1111-4111-8111-111111111111"
	for _, test := range []struct {
		method, resource             string
		query, device, authenticated bool
	}{
		{"GET", "/v1/auth/registration-operations/" + id, true, false, false},
		{"HEAD", "/v1/auth/registration-operations/" + id, true, false, false},
		{"POST", "/v1/auth/sms/challenges", false, true, false},
		{"POST", "/v1/auth/register", false, true, false},
		{"POST", "/v1/auth/password-resets", false, true, false},
		{"POST", "/v1/me/password-changes", false, true, true},
		{"POST", "/v1/me/profile-change-requests/" + id + "/mobile-challenges", false, true, true},
		{"POST", "/v1/me/profile-change-requests/" + id + "/actions/complete", false, true, true},
		{"POST", "/v1/auth/registration/precheck", false, false, false},
		{"POST", "/v1/auth/captcha/challenges", false, false, false},
		{"GET", "/v1/me", false, false, true},
		{"GET", "/v1/me/profile-change-requests/" + id, false, false, true},
		{"POST", "/v1/me/profile-change-requests", false, false, true},
	} {
		t.Run(test.method+test.resource, func(t *testing.T) {
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if (r.Header.Get("X-Registration-Query-Token") == "public-test-operation-proof") != test.query || (r.Header.Get("X-Device-ID") == "public-test-device") != test.device {
					t.Error("operation-specific proof lost or crossed route boundary")
				}
				if r.Header.Get("Cookie") != "" || r.Header.Get("X-Internal-Service-Token") != "" || r.Header.Get("Authorization") == "Bearer browser-value" {
					t.Error("untrusted browser authority was forwarded")
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer upstream.Close()
			store := newMemoryStore()
			service := testService(t, store)
			service.config.AccountURL = parseTestURL(t, upstream.URL)
			request := httptest.NewRequest(test.method, "/api/account"+test.resource, strings.NewReader(`{}`))
			request.Header.Set("Origin", service.config.PublicURL.String())
			request.Header.Set("X-Registration-Query-Token", "public-test-operation-proof")
			request.Header.Set("X-Device-ID", "public-test-device")
			request.Header.Set("X-Internal-Service-Token", "browser-internal-value")
			request.Header.Set("Authorization", "Bearer browser-value")
			if test.authenticated {
				session := validSession(uuid.NewString())
				if err := store.SaveSession(context.Background(), session); err != nil {
					t.Fatal(err)
				}
				request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
				request.AddCookie(&http.Cookie{Name: service.csrfCookieName(), Value: session.CSRFToken})
				request.Header.Set("X-CSRF-Token", session.CSRFToken)
			}
			response := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(response, request)
			if response.Code != http.StatusOK || calls != 1 {
				t.Fatalf("status=%d calls=%d", response.Code, calls)
			}
		})
	}
}

func TestPersonalRiskSourceUsesOnlyPortalPeer(t *testing.T) {
	for _, test := range []struct {
		remote, want string
		status       int
	}{
		{"192.0.2.45:4321", "192.0.2.45", 200},
		{"[2001:db8::1]:4321", "2001:db8::1", 200},
		{"203.0.113.9", "", 400},
		{"spoof.example:4321", "", 400},
	} {
		t.Run(test.remote, func(t *testing.T) {
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("X-Pinvou-Client-Source") != test.want || r.Header.Get("X-Forwarded-For") != "" {
					t.Error("browser-supplied source replaced authoritative peer")
				}
				w.WriteHeader(200)
			}))
			defer upstream.Close()
			service := testService(t, newMemoryStore())
			service.config.AccountURL = parseTestURL(t, upstream.URL)
			request := httptest.NewRequest("POST", "/api/account/v1/auth/sms/challenges", strings.NewReader(`{}`))
			request.RemoteAddr = test.remote
			request.Header.Set("Origin", service.config.PublicURL.String())
			request.Header.Set("X-Pinvou-Client-Source", "203.0.113.99")
			request.Header.Set("X-Forwarded-For", "203.0.113.98")
			response := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(response, request)
			if response.Code != test.status || (test.status == 400 && calls != 0) {
				t.Fatalf("status=%d calls=%d", response.Code, calls)
			}
		})
	}
}
