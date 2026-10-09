package portal

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

func TestF07SignupPolicyProductionPortal(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/admin/v1/registration-bonus-policy" || r.Header.Get("Authorization") != "Bearer test-access" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Internal-Service-Token") != "" {
			t.Error("policy path or authority changed")
		}
		if r.Method == http.MethodPut {
			body, _ := io.ReadAll(r.Body)
			if string(body) != `{"enabled":false,"amount_points":0,"validity_days":30,"expected_revision":1,"reason":"Controlled policy update"}` || r.Header.Get("Idempotency-Key") != "controlled-policy-key" {
				t.Error("policy body or immutable key changed")
			}
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"revision":2,"enabled":false}`)
	}))
	defer upstream.Close()
	store := newMemoryStore()
	service := testService(t, store)
	service.config.BillingURL = parseTestURL(t, upstream.URL)
	session := validSession(uuid.NewString())
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, method, resource string
		session, csrf          bool
		want                   int
	}{
		{"read", "GET", "/admin/v1/registration-bonus-policy", true, false, 200},
		{"write", "PUT", "/admin/v1/registration-bonus-policy", true, true, 200},
		{"no-session", "GET", "/admin/v1/registration-bonus-policy", false, false, 401},
		{"no-csrf", "PUT", "/admin/v1/registration-bonus-policy", true, false, 403},
		{"post-denied", "POST", "/admin/v1/registration-bonus-policy", true, true, 404},
		{"delete-denied", "DELETE", "/admin/v1/registration-bonus-policy", true, true, 404},
		{"subroute-denied", "GET", "/admin/v1/registration-bonus-policy/enable", true, false, 404},
		{"future-denied", "GET", "/admin/v1/account-closed-grants", true, false, 404},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := calls.Load()
			r := httptest.NewRequest(c.method, "/api/admin/billing"+c.resource, strings.NewReader(`{"enabled":false,"amount_points":0,"validity_days":30,"expected_revision":1,"reason":"Controlled policy update"}`))
			r.Header.Set("Authorization", "Bearer forged-browser")
			r.Header.Set("X-Internal-Service-Token", "forged-browser")
			r.Header.Set("Idempotency-Key", "controlled-policy-key")
			if c.session {
				r.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
			}
			if c.csrf {
				r.Header.Set("Origin", service.config.PublicURL.String())
				r.Header.Set("X-CSRF-Token", session.CSRFToken)
				r.AddCookie(&http.Cookie{Name: service.csrfCookieName(), Value: session.CSRFToken})
			}
			w := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(w, r)
			if w.Code != c.want {
				t.Fatalf("real Portal status=%d want=%d", w.Code, c.want)
			}
			if c.want == 200 {
				if calls.Load() != before+1 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), `"revision":2`) {
					t.Fatal("policy response was not the upstream no-store receipt")
				}
			} else if calls.Load() != before {
				t.Fatal("denied policy request reached upstream")
			}
		})
	}
}
