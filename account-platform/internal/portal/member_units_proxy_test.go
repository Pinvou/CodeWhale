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

func TestMemberUnitsActualSessionProxy(t *testing.T) {
	org, member := uuid.NewString(), uuid.NewString()
	for _, admin := range []bool{false, true} {
		name := "organization"
		if admin {
			name = "platform"
		}
		t.Run(name, func(t *testing.T) {
			store := newMemoryStore()
			s := testService(t, store)
			session := validSession(uuid.NewString())
			if err := store.SaveSession(context.Background(), session); err != nil {
				t.Fatal(err)
			}
			path := "/v1/organizations/" + org + "/members/" + member + "/units"
			prefix := "/api/account"
			if admin {
				path = "/admin" + path
				prefix = "/api/admin/account"
			}
			body := `{"unit_ids":[],"expected_version":7}`
			receipt := `{"organization_id":"` + org + `","member":{"user_id":"` + member + `","version":8,"unit_ids":[]}}`
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				raw, err := io.ReadAll(r.Body)
				if err != nil || r.Method != http.MethodPut || r.URL.Path != path || string(raw) != body || r.Header.Get("Idempotency-Key") != "owned-member-units-command" || r.Header.Get("Authorization") != "Bearer "+session.AccessToken || r.Header.Get("X-CSRF-Token") != "" {
					t.Error("proxy changed key/CAS/explicit scope or session authority")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, receipt)
			}))
			defer upstream.Close()
			s.config.AccountURL = parseTestURL(t, upstream.URL)
			r := financeBrowserRequest(s, session, http.MethodPut, prefix+path, body)
			r.Header.Set("Idempotency-Key", "owned-member-units-command")
			r.Header.Set("Authorization", "Bearer browser-spoof")
			w := httptest.NewRecorder()
			s.Handler(nil).ServeHTTP(w, r)
			if w.Code != 200 || w.Body.String() != receipt || calls.Load() != 1 {
				t.Fatalf("actual member units proxy status=%d calls=%d", w.Code, calls.Load())
			}
			for _, tc := range []struct {
				name, method, target string
				csrf                 bool
				want                 int
			}{
				{"csrf", http.MethodPut, prefix + path, true, 403},
				{"wrong-method", http.MethodPost, prefix + path, false, 404},
				{"read-not-exposed", http.MethodGet, prefix + path, false, 404},
				{"future-child", http.MethodPut, prefix + path + "/reclaim", false, 404},
				{"invalid-member", http.MethodPut, prefix + path[:len(path)-len(member)-len("/units")] + "00000000-0000-0000-0000-000000000000/units", false, 404},
			} {
				t.Run(tc.name, func(t *testing.T) {
					r := financeBrowserRequest(s, session, tc.method, tc.target, body)
					if tc.csrf {
						r.Header.Set("X-CSRF-Token", "wrong")
					}
					w := httptest.NewRecorder()
					s.Handler(nil).ServeHTTP(w, r)
					if w.Code != tc.want || calls.Load() != 1 {
						t.Fatal("denied member operation reached Account authority", w.Code, calls.Load())
					}
				})
			}
		})
	}
}
