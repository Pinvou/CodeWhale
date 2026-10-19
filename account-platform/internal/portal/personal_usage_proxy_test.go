package portal

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

func TestF05PersonalUsageThroughProductionPortal(t *testing.T) {
	var calls atomic.Int32
	var facts sync.Mutex
	var lastPath, lastQuery, lastKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		facts.Lock()
		defer facts.Unlock()
		calls.Add(1)
		lastPath, lastQuery, lastKey = r.URL.Path, r.URL.RawQuery, r.Header.Get("Idempotency-Key")
		if r.Header.Get("Authorization") != "Bearer test-access" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Internal-Service-Token") != "" || r.Header.Get("X-Pinvou-Token-Account-ID") != "" {
			t.Error("browser authority escaped or server session bearer was lost")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("controlled_denial") == "true" {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"code":"FORBIDDEN"}`)
			return
		}
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
		}
		io.WriteString(w, `{"controlled_token_response":true}`)
	}))
	defer upstream.Close()
	store := newMemoryStore()
	service := testService(t, store)
	service.config.TokenGatewayURL = parseTestURL(t, upstream.URL)
	session := validSession(uuid.NewString())
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	id := "11111111-1111-4111-8111-111111111111"
	cases := []struct {
		name, method, path string
		want               int
		session, csrf      bool
	}{
		{"catalog", "GET", "/v1/models", 200, true, true},
		{"catalog-head", "HEAD", "/v1/models", 200, true, true},
		{"turn-page", "GET", "/v1/me/usage-turns?limit=20&cursor=turn-v1.token", 200, true, true},
		{"request-fallback-page", "GET", "/v1/me/usage-requests?limit=20&cursor=request-page", 200, true, true},
		{"subject-owned-cancel", "POST", "/v1/usage-requests/" + id + "/cancel", 202, true, true},
		{"cancel-downstream-denial", "POST", "/v1/usage-requests/" + id + "/cancel?controlled_denial=true", 403, true, true},
		{"cancel-csrf-rejected", "POST", "/v1/usage-requests/" + id + "/cancel", 403, true, false},
		{"history-session-required", "GET", "/v1/me/usage-turns", 401, false, false},
		{"finance-retained", "GET", "/v1/me/balance", 200, true, true},
		{"cancel-get-denied", "GET", "/v1/usage-requests/" + id + "/cancel", 404, true, true},
		{"cancel-nil-id-denied", "POST", "/v1/usage-requests/00000000-0000-0000-0000-000000000000/cancel", 404, true, true},
		{"cancel-invalid-id-denied", "POST", "/v1/usage-requests/not-a-uuid/cancel", 404, true, true},
		{"cancel-suffix-denied", "POST", "/v1/usage-requests/" + id + "/cancel/extra", 404, true, true},
		{"catalog-write-denied", "POST", "/v1/models", 404, true, true},
		{"history-write-denied", "POST", "/v1/me/usage-requests", 404, true, true},
		{"chat-proxy-denied", "POST", "/v1/chat/completions", 404, true, true},
		{"organization-future-denied", "GET", "/v1/organizations/" + id + "/models", 404, true, true},
		{"internal-readiness-denied", "GET", "/internal/v1/client-readiness", 404, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prior := calls.Load()
			req := httptest.NewRequest(c.method, "/api/token"+c.path, strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer browser-forged")
			req.Header.Set("X-Internal-Service-Token", "browser-forged")
			req.Header.Set("X-Pinvou-Token-Account-ID", id)
			req.Header.Set("Idempotency-Key", "stable-cancel-key")
			if c.session {
				req.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
			}
			if c.csrf {
				req.Header.Set("Origin", service.config.PublicURL.String())
				req.Header.Set("X-CSRF-Token", session.CSRFToken)
				req.AddCookie(&http.Cookie{Name: service.csrfCookieName(), Value: session.CSRFToken})
			}
			out := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(out, req)
			if out.Code != c.want {
				t.Fatalf("actual Portal status=%d want=%d body=%s", out.Code, c.want, out.Body.String())
			}
			dispatched := c.want == 200 || c.want == 202 || c.name == "cancel-downstream-denial"
			if !dispatched {
				if calls.Load() != prior {
					t.Fatal("denied request reached Token")
				}
				return
			}
			facts.Lock()
			defer facts.Unlock()
			if calls.Load() != prior+1 || lastPath != req.URL.Path[len("/api/token"):] || lastQuery != req.URL.RawQuery || out.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("Token path/query/status delivery changed")
			}
			if c.method == "POST" && lastKey != "stable-cancel-key" {
				t.Fatal("cancel key was lost")
			}
		})
	}
}
