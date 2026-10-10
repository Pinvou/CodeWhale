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

// The real BFF preserves current-session authority. Account owns permission
// and recent-auth decisions; this controlled upstream verifies their receipts
// are forwarded unchanged, not guessed by the BFF.
func TestEntitlementPrecheckActualSessionProxy(t *testing.T) {
	const resource = "/admin/v2/approval-command-prechecks"
	body := `{"command_type":"ENTITLEMENT_COMPONENT_ADJUSTMENT","target_resource_id":"` + uuid.NewString() + `","payment_order_id":"` + uuid.NewString() + `","reason_code":"PRODUCT_SPLIT_CORRECTION"}`
	for _, status := range []int{200, 403, 401, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
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
				if err != nil || string(raw) != body || r.Method != http.MethodPost || r.URL.Path != resource || r.Header.Get("Authorization") != "Bearer "+session.AccessToken || r.Header.Get("X-CSRF-Token") != "" {
					t.Error("proxy changed frozen request or trusted session")
				}
				w.Header().Set("Cache-Control", "no-store")
				w.WriteHeader(status)
				io.WriteString(w, `{"controlled_authority":true}`)
			}))
			defer upstream.Close()
			service.config.AccountURL = parseTestURL(t, upstream.URL)
			path := "/api/admin/account" + resource
			request := financeBrowserRequest(service, session, "POST", path, body)
			request.Header.Set("Authorization", "Bearer browser-spoof")
			out := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(out, request)
			if out.Code != status || calls.Load() != 1 {
				t.Fatalf("status=%d calls=%d", out.Code, calls.Load())
			}
			for _, tc := range []struct {
				name, method, suffix string
				session, csrf        bool
				status               int
			}{
				{"csrf", "POST", "", true, false, 403}, {"no-session", "POST", "", false, false, 401},
				{"get", "GET", "", true, true, 404}, {"put", "PUT", "", true, true, 404}, {"extra", "POST", "/force", true, true, 404},
			} {
				t.Run(tc.name, func(t *testing.T) {
					r := financeBrowserRequest(service, session, tc.method, path+tc.suffix, body)
					if !tc.csrf {
						r.Header.Set("X-CSRF-Token", "wrong")
					}
					if !tc.session {
						r = httptest.NewRequest(tc.method, path+tc.suffix, strings.NewReader(body))
					}
					result := httptest.NewRecorder()
					service.Handler(nil).ServeHTTP(result, r)
					if result.Code != tc.status || calls.Load() != 1 {
						t.Fatalf("negative status=%d calls=%d", result.Code, calls.Load())
					}
				})
			}
		})
	}
}
