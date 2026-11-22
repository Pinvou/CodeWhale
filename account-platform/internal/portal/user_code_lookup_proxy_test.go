package portal

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestUserCodeLookupActualSessionProxy(t *testing.T) {
	const code = "USR-1111-1111-1111-1111-11"
	for _, status := range []int{200, 404, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			store := newMemoryStore()
			service := testService(t, store)
			session := validSession(uuid.NewString())
			if e := store.SaveSession(context.Background(), session); e != nil {
				t.Fatal(e)
			}
			var calls atomic.Int32
			body := `{"user_id":"` + uuid.NewString() + `","user_code":"` + code + `","status":"DELETED"}`
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "GET" || r.URL.Path != "/admin/v1/user-code-lookups/"+code || r.Header.Get("Authorization") != "Bearer "+session.AccessToken || r.Header.Get("X-Pinvou-Client-Source") != "192.0.2.24" || r.Header.Get("X-Device-ID") != "controlled-device" {
					t.Errorf("lookup authority/risk/identity changed: method=%s", r.Method)
				}
				w.Header().Set("Cache-Control", "no-store")
				w.WriteHeader(status)
				fmt.Fprint(w, body)
			}))
			defer upstream.Close()
			service.config.AccountURL = parseTestURL(t, upstream.URL)
			path := "/api/admin/account/admin/v1/user-code-lookups/" + code
			req := financeBrowserRequest(service, session, "GET", path, "")
			req.RemoteAddr = "192.0.2.24:1234"
			req.Header.Set("Authorization", "Bearer browser-spoof")
			req.Header.Set("X-Pinvou-Client-Source", "203.0.113.9")
			req.Header.Set("X-Device-ID", "controlled-device")
			out := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(out, req)
			if out.Code != status || out.Body.String() != body || calls.Load() != 1 {
				t.Fatal("actual proxy result", out.Code, out.Body.String(), calls.Load())
			}
			for _, method := range []string{"HEAD", "POST", "PUT", "PATCH", "DELETE"} {
				r := financeBrowserRequest(service, session, method, path, "")
				w := httptest.NewRecorder()
				service.Handler(nil).ServeHTTP(w, r)
				if w.Code != 404 || calls.Load() != 1 {
					t.Fatal("lookup gained unsafe method", method, w.Code, calls.Load())
				}
			}
			for _, suffix := range []string{"/extra", "/batch", "%2Fbad"} {
				r := financeBrowserRequest(service, session, "GET", path+suffix, "")
				w := httptest.NewRecorder()
				service.Handler(nil).ServeHTTP(w, r)
				if w.Code != 404 || calls.Load() != 1 {
					t.Fatal("lookup path broadened", suffix, w.Code)
				}
			}
			r := httptest.NewRequest("GET", path, strings.NewReader(""))
			w := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(w, r)
			if w.Code != 401 || calls.Load() != 1 {
				t.Fatal("no session lookup reached owner", w.Code)
			}
		})
	}
}
