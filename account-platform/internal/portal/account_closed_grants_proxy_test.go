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

// This exercises the real Portal session and CSRF proxy against a controlled
// Token authority; Token's organization permission check remains upstream.
func TestAccountClosedGrantActualSessionProxy(t *testing.T) {
	org, grant := uuid.NewString(), uuid.NewString()
	base := "/v1/organizations/" + org
	for _, tc := range []struct {
		name, path, body string
		status           int
	}{
		{"single", base + "/account-closed-grants/" + grant + "/reclaim", `{"reason":"ACCOUNT_CLOSED_FULL_RECLAIM"}`, 200},
		{"batch", base + "/account-closed-grants:reclaim-batch", `{"grant_ids":["` + grant + `"],"reason":"ACCOUNT_CLOSED_FULL_RECLAIM"}`, 207},
		{"permission-denied", base + "/account-closed-grants/" + grant + "/reclaim", `{"reason":"ACCOUNT_CLOSED_FULL_RECLAIM"}`, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemoryStore()
			service := testService(t, store)
			session := validSession(uuid.NewString())
			if err := store.SaveSession(context.Background(), session); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				raw, err := io.ReadAll(r.Body)
				if err != nil || string(raw) != tc.body || r.Method != "POST" || r.URL.Path != tc.path || r.Header.Get("Authorization") != "Bearer "+session.AccessToken || r.Header.Get("Idempotency-Key") != "original-closed-grant-key" || r.Header.Get("X-CSRF-Token") != "" {
					t.Error("proxy changed original facts, key or current session authority")
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"controlled":true}`)
			}))
			defer upstream.Close()
			service.config.TokenGatewayURL = parseTestURL(t, upstream.URL)
			request := financeBrowserRequest(service, session, "POST", "/api/token"+tc.path, tc.body)
			request.Header.Set("Idempotency-Key", "original-closed-grant-key")
			request.Header.Set("Authorization", "Bearer browser-spoof")
			out := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(out, request)
			if out.Code != tc.status || calls.Load() != 1 {
				t.Fatalf("proxy status=%d calls=%d", out.Code, calls.Load())
			}
			for _, negative := range []struct {
				name, method, path string
				csrf, session      bool
				status             int
			}{
				{"csrf", "POST", tc.path, false, true, 403},
				{"no-session", "POST", tc.path, false, false, 401},
				{"read-method", "GET", tc.path, true, true, 404},
				{"extra-segment", "POST", tc.path + "/force", true, true, 404},
				{"legacy-wrong-path", "POST", base + "/grants/" + grant + "/account-closed-reclaim", true, true, 404},
			} {
				t.Run(negative.name, func(t *testing.T) {
					r := financeBrowserRequest(service, session, negative.method, "/api/token"+negative.path, tc.body)
					if !negative.csrf {
						r.Header.Set("X-CSRF-Token", "wrong")
					}
					if !negative.session {
						r = httptest.NewRequest(negative.method, "/api/token"+negative.path, nil)
					}
					out := httptest.NewRecorder()
					service.Handler(nil).ServeHTTP(out, r)
					if out.Code != negative.status || calls.Load() != 1 {
						t.Fatalf("negative status=%d calls=%d", out.Code, calls.Load())
					}
				})
			}
		})
	}
}
