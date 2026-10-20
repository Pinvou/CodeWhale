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

// Actual Portal router/session/CSRF forwarding, with a controlled Token HTTP
// authority. Domain membership, DB effects and lease ownership are not mocked
// as proven here: Token remains responsible for those authoritative decisions.
func TestRetainedClosedGrantsProductionProxy(t *testing.T) {
	org, grant := uuid.NewString(), uuid.NewString()
	one := "/v1/organizations/" + org + "/account-closed-grants/" + grant + "/reclaim"
	batch := "/v1/organizations/" + org + "/account-closed-grants:reclaim-batch"
	for _, tc := range []struct {
		name, method, resource string
		session, csrf          bool
		want                   int
	}{
		{"single", "POST", one, true, true, 200},
		{"batch", "POST", batch, true, true, 200},
		{"no-session", "POST", one, false, false, 401},
		{"no-csrf", "POST", batch, true, false, 403},
		{"wrong-method", "GET", one, true, true, 404},
		{"personal-scope", "POST", "/v1/me/account-closed-grants/" + grant + "/reclaim", true, true, 404},
		{"foreign-admin-scope", "POST", "/admin/v1/organizations/" + org + "/account-closed-grants:reclaim-batch", true, true, 404},
		{"nil-grant", "POST", "/v1/organizations/" + org + "/account-closed-grants/00000000-0000-0000-0000-000000000000/reclaim", true, true, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemoryStore()
			service := testService(t, store)
			session := validSession(uuid.NewString())
			if err := store.SaveSession(context.Background(), session); err != nil {
				t.Fatal(err)
			}
			const body = `{"reason":"ACCOUNT_CLOSED","expected_version":9007199254740993}`
			var calls atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				raw, err := io.ReadAll(r.Body)
				if err != nil || r.Method != tc.method || r.URL.Path != tc.resource || string(raw) != body || r.Header.Get("Idempotency-Key") != "retained-reclaim-key" || r.Header.Get("Authorization") != "Bearer "+session.AccessToken || r.Header.Get("Cookie") != "" || r.Header.Get("X-CSRF-Token") != "" {
					t.Error("reclaim proxy changed owner tuple/key/raw integer or session authority")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"controlled_receipt":true}`)
			}))
			defer up.Close()
			service.config.TokenGatewayURL = parseTestURL(t, up.URL)
			request := financeBrowserRequest(service, session, tc.method, "/api/token"+tc.resource, body)
			request.Header.Set("Idempotency-Key", "retained-reclaim-key")
			request.Header.Set("Authorization", "Bearer forged-browser")
			if !tc.session {
				request.Header.Del("Cookie")
			}
			if !tc.csrf {
				request.Header.Del("X-CSRF-Token")
			}
			out := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(out, request)
			wantCalls := int32(0)
			if tc.want == 200 {
				wantCalls = 1
			}
			if out.Code != tc.want || calls.Load() != wantCalls || tc.want == 200 && !strings.Contains(out.Body.String(), "controlled_receipt") {
				t.Fatalf("proxy status=%d calls=%d want=%d/%d", out.Code, calls.Load(), tc.want, wantCalls)
			}
		})
	}
}
