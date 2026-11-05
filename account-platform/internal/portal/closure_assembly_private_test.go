package portal

import (
	"context"
	"github.com/google/uuid"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestClosureActualSessionProxy(t *testing.T) {
	id := uuid.NewString()
	for _, tc := range []struct {
		name, method, path, body string
		status                   int
	}{
		{"create", "POST", "/api/account/v1/me/account-closure-requests", `{"confirmation":"CLOSE_MY_ACCOUNT"}`, 202},
		{"read", "GET", "/api/account/v1/me/account-closure-request", "", 200},
		{"upstream-forbidden", "POST", "/api/account/v1/me/account-closure-requests", `{"confirmation":"CLOSE_MY_ACCOUNT"}`, 403},
		{"head", "HEAD", "/api/account/v1/me/account-closure-request", "", 200},
		{"list", "GET", "/api/admin/account/admin/v1/account-closure-requests", "", 200},
		{"admin-head", "HEAD", "/api/admin/account/admin/v1/account-closure-requests", "", 200},
		{"retry", "POST", "/api/admin/account/admin/v1/account-closure-requests/" + id + "/actions/retry", `{"reason":"owned repair reason"}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemoryStore()
			s := testService(t, store)
			session := validSession(uuid.NewString())
			if err := store.SaveSession(context.Background(), session); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				raw, e := io.ReadAll(r.Body)
				if e != nil || r.Method != tc.method || string(raw) != tc.body || r.Header.Get("Authorization") != "Bearer "+session.AccessToken || r.Header.Get("X-CSRF-Token") != "" {
					t.Error("session/raw/method authority changed")
				}
				if tc.method == "POST" && r.Header.Get("Idempotency-Key") != "original-closure-key" {
					t.Error("key changed")
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"controlled":true}`)
			}))
			defer up.Close()
			s.config.AccountURL = parseTestURL(t, up.URL)
			r := financeBrowserRequest(s, session, tc.method, tc.path, tc.body)
			r.Header.Set("Idempotency-Key", "original-closure-key")
			r.Header.Set("Authorization", "Bearer browser-spoof")
			out := httptest.NewRecorder()
			s.Handler(nil).ServeHTTP(out, r)
			if out.Code != tc.status || calls.Load() != 1 {
				t.Fatalf("proxy %d calls%d", out.Code, calls.Load())
			}
			if tc.method == "POST" {
				t.Run("csrf", func(t *testing.T) {
					r := financeBrowserRequest(s, session, tc.method, tc.path, tc.body)
					r.Header.Set("X-CSRF-Token", "wrong")
					out := httptest.NewRecorder()
					s.Handler(nil).ServeHTTP(out, r)
					if out.Code != 403 || calls.Load() != 1 {
						t.Fatal("CSRF bypass", out.Code)
					}
				})
			}
			t.Run("no-session", func(t *testing.T) {
				r := httptest.NewRequest(tc.method, tc.path, nil)
				out := httptest.NewRecorder()
				s.Handler(nil).ServeHTTP(out, r)
				if out.Code != 401 || calls.Load() != 1 {
					t.Fatal("session bypass", out.Code)
				}
			})
		})
	}
	for _, path := range []string{"/api/account/v1/me/account-closure-requests/force", "/api/admin/account/admin/v1/users/" + id, "/api/admin/account/admin/v1/account-closure-requests/00000000-0000-0000-0000-000000000000/actions/retry"} {
		t.Run("denied-"+path, func(t *testing.T) {
			store := newMemoryStore()
			s := testService(t, store)
			session := validSession(uuid.NewString())
			_ = store.SaveSession(context.Background(), session)
			r := financeBrowserRequest(s, session, http.MethodDelete, path, "")
			out := httptest.NewRecorder()
			s.Handler(nil).ServeHTTP(out, r)
			if out.Code != 404 {
				t.Fatal("unpublished DELETE exposed", out.Code)
			}
		})
	}
}
