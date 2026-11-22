package portal

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

func TestPlatformRolesActualSessionProxy(t *testing.T) {
	for _, item := range []struct {
		method, path string
		status       int
	}{{"GET", "/admin/v1/roles", 200}, {"GET", "/admin/v1/role-bindings", 200}, {"POST", "/admin/v1/role-bindings", 201}, {"DELETE", "/admin/v1/role-bindings/" + uuid.NewString(), 200}} {
		t.Run(item.method+item.path, func(t *testing.T) {
			store := newMemoryStore()
			s := testService(t, store)
			session := validSession(uuid.NewString())
			if err := store.SaveSession(context.Background(), session); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != item.method || r.URL.Path != item.path || r.Header.Get("Authorization") != "Bearer "+session.AccessToken || r.Header.Get("Cookie") != "" {
					t.Error("wrong upstream identity/path")
				}
				w.WriteHeader(item.status)
				fmt.Fprint(w, `{"controlled":true}`)
			}))
			defer upstream.Close()
			s.config.AccountURL = parseTestURL(t, upstream.URL)
			path := "/api/admin/account" + item.path
			request := financeBrowserRequest(s, session, item.method, path, `{"controlled":true}`)
			out := httptest.NewRecorder()
			s.Handler(nil).ServeHTTP(out, request)
			if out.Code != item.status || calls.Load() != 1 {
				t.Fatal(out.Code, out.Body.String(), calls.Load())
			}
			if item.method != "GET" {
				request = financeBrowserRequest(s, session, item.method, path, `{}`)
				request.Header.Del("X-CSRF-Token")
				out = httptest.NewRecorder()
				s.Handler(nil).ServeHTTP(out, request)
				if out.Code < 400 || calls.Load() != 1 {
					t.Fatal("write lacked CSRF gate", out.Code, calls.Load())
				}
			}
			request = httptest.NewRequest(item.method, path, strings.NewReader(`{}`))
			out = httptest.NewRecorder()
			s.Handler(nil).ServeHTTP(out, request)
			if out.Code != 401 || calls.Load() != 1 {
				t.Fatal("anonymous reached account", out.Code, calls.Load())
			}
		})
	}
	for _, item := range []struct{ method, path string }{{"POST", "/admin/v1/roles"}, {"PUT", "/admin/v1/role-bindings"}, {"PATCH", "/admin/v1/role-bindings/" + uuid.NewString()}, {"DELETE", "/admin/v1/role-bindings/not-a-uuid"}, {"DELETE", "/admin/v1/role-bindings/" + uuid.NewString() + "/extra"}} {
		if accountAdminPathAllowed(item.method, item.path) {
			t.Fatal("unrelated mutation allowed", item)
		}
	}
}
