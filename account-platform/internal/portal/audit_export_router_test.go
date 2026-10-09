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

func TestF17AuditPortalOpaqueDownloadAndAuthority(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	session := validSession(uuid.NewString())
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	var calls atomic.Int32
	payload := `{"format":"PINVOU_AUDIT_ENVELOPE_V1","ciphertext":"controlled-opaque"}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+session.AccessToken || r.Header.Get("Cookie") != "" || r.Header.Get("X-CSRF-Token") != "" {
			t.Error("browser authority leaked upstream")
		}
		if r.URL.Path == "/admin/v1/audit-exports/"+id+"/download" {
			if r.Method != "GET" || r.Header.Get("X-Pinvou-Download-Token") != "controlled-one-use-proof" {
				t.Error("opaque proof lost")
			}
			w.Header().Set("Content-Type", "application/vnd.pinvou.audit-envelope+json")
			w.Header().Set("Content-Disposition", `attachment; filename="controlled.pae"`)
			_, _ = io.WriteString(w, payload)
		} else {
			if r.Header.Get("X-Pinvou-Download-Token") != "" {
				t.Error("download proof crossed route")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"items":[]}`)
		}
	}))
	defer upstream.Close()
	service.config.AccountURL = parseTestURL(t, upstream.URL)
	t.Run("exact opaque download", func(t *testing.T) {
		r := financeBrowserRequest(service, session, "GET", "/api/admin/account/admin/v1/audit-exports/"+id+"/download", "")
		r.Header.Set("Authorization", "Bearer browser-untrusted")
		r.Header.Set("X-Pinvou-Download-Token", "controlled-one-use-proof")
		w := httptest.NewRecorder()
		service.Handler(nil).ServeHTTP(w, r)
		if w.Code != 200 || w.Body.String() != payload || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Disposition") != `attachment; filename="controlled.pae"` {
			t.Fatalf("opaque download changed: %d", w.Code)
		}
	})
	t.Run("read strips proof", func(t *testing.T) {
		r := financeBrowserRequest(service, session, "GET", "/api/admin/account/admin/v1/audit-logs", "")
		r.Header.Set("X-Pinvou-Download-Token", "must-not-leak")
		w := httptest.NewRecorder()
		service.Handler(nil).ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
	})
	t.Run("HEAD cannot consume", func(t *testing.T) {
		before := calls.Load()
		r := financeBrowserRequest(service, session, "HEAD", "/api/admin/account/admin/v1/audit-exports/"+id+"/download", "")
		w := httptest.NewRecorder()
		service.Handler(nil).ServeHTTP(w, r)
		if w.Code != 404 || calls.Load() != before {
			t.Fatal("HEAD reached owner")
		}
	})
	t.Run("CSRF rejects create", func(t *testing.T) {
		before := calls.Load()
		r := financeBrowserRequest(service, session, "POST", "/api/admin/account/admin/v1/audit-exports", `{}`)
		r.Header.Set("X-CSRF-Token", "wrong")
		w := httptest.NewRecorder()
		service.Handler(nil).ServeHTTP(w, r)
		if w.Code != 403 || calls.Load() != before {
			t.Fatal("CSRF reached owner")
		}
	})
}
