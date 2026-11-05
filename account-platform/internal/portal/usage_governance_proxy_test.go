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

func TestF05UsageGovernanceThroughProductionPortal(t *testing.T) {
	var calls atomic.Int32
	var mu sync.Mutex
	var receivedPath, receivedQuery, receivedKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls.Add(1)
		receivedPath, receivedQuery, receivedKey = r.URL.Path, r.URL.RawQuery, r.Header.Get("Idempotency-Key")
		if r.Header.Get("Authorization") != "Bearer test-access" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Internal-Service-Token") != "" || r.Header.Get("X-Pinvou-Approval-Proof-Test") != "" {
			t.Error("browser authority forwarded or server session lost")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("denied") == "true" {
			w.WriteHeader(403)
			io.WriteString(w, `{"code":"FORBIDDEN"}`)
			return
		}
		if r.Method == "POST" {
			w.WriteHeader(201)
		}
		io.WriteString(w, `{"controlled_token_response":true}`)
	}))
	defer upstream.Close()
	store := newMemoryStore()
	service := testService(t, store)
	service.config.TokenGatewayURL = parseTestURL(t, upstream.URL)
	service.config.AdminTokenDetailsEnabled = true
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
		{"list", "GET", "/admin/v1/usage-requests?limit=20&cursor=usage-page", 200, true, true},
		{"detail", "GET", "/admin/v1/usage-requests/" + id, 200, true, true},
		{"evidence", "GET", "/admin/v1/usage-requests/" + id + "/evidence", 200, true, true},
		{"reconciliation", "GET", "/admin/v1/reconciliation-cases?limit=20", 200, true, true},
		{"readiness", "GET", "/admin/v1/commands/TOKEN_USAGE_RECONCILIATION_RETRY/readiness", 200, true, true},
		{"readiness-head", "HEAD", "/admin/v1/commands/TOKEN_USAGE_RECONCILIATION_RETRY/readiness", 200, true, true},
		{"downstream-permission-denial", "GET", "/admin/v1/usage-requests/" + id + "?denied=true", 403, true, true},
		{"required-session", "GET", "/admin/v1/usage-requests", 401, false, false},
		{"existing-precheck", "POST", "/admin/v1/commands:precheck", 201, true, true},
		{"existing-command", "POST", "/admin/v1/commands", 201, true, true},
		{"existing-command-result", "GET", "/admin/v1/commands/" + id, 200, true, true},
		{"existing-receipt-key", "GET", "/admin/v1/commands/by-idempotency-key?command_type=TOKEN_USAGE_RECONCILIATION_RETRY", 200, true, true},
		{"existing-command-csrf", "POST", "/admin/v1/commands", 403, true, false},
		{"invalid-usage-id", "GET", "/admin/v1/usage-requests/not-a-uuid", 404, true, true},
		{"nil-usage-id", "GET", "/admin/v1/usage-requests/00000000-0000-0000-0000-000000000000/evidence", 404, true, true},
		{"usage-write-denied", "POST", "/admin/v1/usage-requests", 404, true, true},
		{"evidence-write-denied", "POST", "/admin/v1/usage-requests/" + id + "/evidence", 404, true, true},
		{"unknown-readiness-command-denied", "GET", "/admin/v1/commands/FUTURE_COMMAND/readiness", 404, true, true},
		{"uninstalled-usage-subroute-denied", "GET", "/admin/v1/usage-requests/" + id + "/reconciliation", 404, true, true},
		{"future-org-denied", "GET", "/admin/v1/organizations", 404, true, true},
		{"future-operations-denied", "GET", "/admin/v1/model-operations", 404, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := calls.Load()
			req := httptest.NewRequest(c.method, "/api/admin/token"+c.path, strings.NewReader(`{"command_type":"TOKEN_USAGE_RECONCILIATION_RETRY"}`))
			req.Header.Set("Authorization", "Bearer forged")
			req.Header.Set("X-Internal-Service-Token", "forged")
			req.Header.Set("X-Pinvou-Approval-Proof-Test", "forged")
			req.Header.Set("Idempotency-Key", "immutable-command-key")
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
			dispatched := c.want == 200 || c.want == 201 || c.name == "downstream-permission-denial"
			if !dispatched {
				if calls.Load() != before {
					t.Fatal("denied request reached Token")
				}
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if calls.Load() != before+1 || receivedPath != req.URL.Path[len("/api/admin/token"):] || receivedQuery != req.URL.RawQuery || receivedKey != "immutable-command-key" || out.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("raw Token path/query/key/receipt changed")
			}
		})
	}
}
