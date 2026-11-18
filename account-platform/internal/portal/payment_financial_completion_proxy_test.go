package portal

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

func TestPaymentFinancialCompletionProxy(t *testing.T) {
	base := "/admin/v1/payment-anomalies/" + uuid.NewString() + "/financial-completion-proposals"
	for _, path := range []string{base, base + "/" + uuid.NewString() + "/confirm"} {
		t.Run(path, func(t *testing.T) {
			store := newMemoryStore()
			service := testService(t, store)
			session := validSession(uuid.NewString())
			if err := store.SaveSession(context.Background(), session); err != nil {
				t.Fatal(err)
			}
			body := `{"proof_kind":"REFUND","reference_id":"11111111-1111-4111-8111-111111111111","reason":"controlled completion facts"}`
			if path != base {
				body = `{"reason":"controlled independent confirmation"}`
			}
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				raw, err := io.ReadAll(r.Body)
				if err != nil || string(raw) != body || r.URL.Path != path || r.Header.Get("Authorization") != "Bearer "+session.AccessToken || r.Header.Get("Idempotency-Key") != "controlled-original-completion" || r.Header.Get("Cookie") != "" || r.Header.Get("X-CSRF-Token") != "" {
					t.Error("completion identity or immutable facts changed")
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"case_id":"11111111-1111-4111-8111-111111111111"}`)
			}))
			defer upstream.Close()
			service.config.BillingURL = parseTestURL(t, upstream.URL)
			target := "/api/admin/billing" + path
			request := financeBrowserRequest(service, session, http.MethodPost, target, body)
			request.Header.Set("Authorization", "Bearer untrusted-browser-token")
			request.Header.Set("Idempotency-Key", "controlled-original-completion")
			response := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(response, request)
			if response.Code != 200 || calls.Load() != 1 || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("completion not forwarded through actual BFF", response.Code, response.Body.String())
			}
			for _, attack := range []string{"csrf", "origin", "session"} {
				request = financeBrowserRequest(service, session, http.MethodPost, target, body)
				switch attack {
				case "csrf":
					request.Header.Set("X-CSRF-Token", "wrong")
				case "origin":
					request.Header.Set("Origin", "https://untrusted.example")
				case "session":
					request.Header.Set("Cookie", "")
				}
				response = httptest.NewRecorder()
				service.Handler(nil).ServeHTTP(response, request)
				if (response.Code != 403 && response.Code != 401) || calls.Load() != 1 {
					t.Fatal("untrusted completion reached Billing", attack, response.Code)
				}
			}
			for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
				if billingAdminPathAllowed(method, path) {
					t.Fatal("unexpected completion method", method)
				}
			}
		})
	}
	for _, path := range []string{base + "/not-a-uuid/confirm", base + "/00000000-0000-0000-0000-000000000000/confirm", base + "/" + uuid.NewString() + "/force", base + "/extra"} {
		if billingAdminPathAllowed(http.MethodPost, path) {
			t.Fatal("malformed/future operation admitted", path)
		}
	}
}
