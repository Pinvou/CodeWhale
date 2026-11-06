package portal

import (
	"bytes"
	"context"
	"github.com/google/uuid"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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
		w := newAuditDeadlineRecorder()
		service.Handler(nil).ServeHTTP(w, r)
		if w.Code != 200 || w.Body.String() != payload || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Disposition") != `attachment; filename="controlled.pae"` {
			t.Fatalf("opaque download changed: %d", w.Code)
		}
	})
	t.Run("read strips proof", func(t *testing.T) {
		r := financeBrowserRequest(service, session, "GET", "/api/admin/account/admin/v1/audit-logs", "")
		r.Header.Set("X-Pinvou-Download-Token", "must-not-leak")
		w := newAuditDeadlineRecorder()
		service.Handler(nil).ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
	})
	t.Run("HEAD cannot consume", func(t *testing.T) {
		before := calls.Load()
		r := financeBrowserRequest(service, session, "HEAD", "/api/admin/account/admin/v1/audit-exports/"+id+"/download", "")
		w := newAuditDeadlineRecorder()
		service.Handler(nil).ServeHTTP(w, r)
		if w.Code != 404 || calls.Load() != before {
			t.Fatal("HEAD reached owner")
		}
	})
	t.Run("CSRF rejects create", func(t *testing.T) {
		before := calls.Load()
		r := financeBrowserRequest(service, session, "POST", "/api/admin/account/admin/v1/audit-exports", `{}`)
		r.Header.Set("X-CSRF-Token", "wrong")
		w := newAuditDeadlineRecorder()
		service.Handler(nil).ServeHTTP(w, r)
		if w.Code != 403 || calls.Load() != before {
			t.Fatal("CSRF reached owner")
		}
	})
}

func TestF17AuditDownloadStreamsBeyondOrdinaryAPILimit(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	session := validSession(uuid.NewString())
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	payload := bytes.Repeat([]byte("x"), (33<<20)+17)
	payload[len(payload)-1] = 'z'
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.Header().Set("Content-Disposition", `attachment; filename="large.pae"`)
		if r.URL.Path == "/admin/v1/audit-exports/"+id+"/download" {
			if r.Header.Get("X-Pinvou-Download-Token") != "single-use-proof" {
				t.Error("download proof missing")
			}
			w.Header().Set("Content-Type", "application/vnd.pinvou.audit-envelope+json")
		}
		_, _ = w.Write(payload)
	}))
	defer upstream.Close()
	service.config.AccountURL = parseTestURL(t, upstream.URL)
	t.Run("whole one-use payload", func(t *testing.T) {
		r := financeBrowserRequest(service, session, "GET", "/api/admin/account/admin/v1/audit-exports/"+id+"/download", "")
		r.Header.Set("X-Pinvou-Download-Token", "single-use-proof")
		w := newAuditDeadlineRecorder()
		service.Handler(nil).ServeHTTP(w, r)
		if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), payload) || w.Header().Get("Content-Length") != strconv.Itoa(len(payload)) || w.Header().Get("Content-Disposition") != `attachment; filename="large.pae"` || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("one-use payload or download metadata was truncated")
		}
	})
	t.Run("oversized ordinary API response rejected", func(t *testing.T) {
		r := financeBrowserRequest(service, session, "GET", "/api/admin/account/admin/v1/users", "")
		w := newAuditDeadlineRecorder()
		service.Handler(nil).ServeHTTP(w, r)
		if w.Code != http.StatusBadGateway || !bytes.Contains(w.Body.Bytes(), []byte(`"code":"UPSTREAM_UNAVAILABLE"`)) || w.Header().Get("Content-Disposition") != "" || w.Header().Get("Content-Length") != "" {
			t.Fatal("audit download exception escaped its exact route")
		}
	})
}

// The actual installed BFF forwards the wildcard suffix unchanged. This tests
// six concrete routes and session authority, independently of the OpenAPI text
// and the client-controlled response contract; no external issuer is simulated.
func TestF17AuditPortalSixConcreteTransportPaths(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	session := validSession(uuid.NewString())
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	type captured struct{ method, path, authorization, cookie, csrf, download string }
	requests := make(chan captured, 6)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- captured{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Cookie"), r.Header.Get("X-CSRF-Token"), r.Header.Get("X-Pinvou-Download-Token")}
		if auditExportDownloadPath(r.Method, r.URL.Path) {
			w.Header().Set("Content-Type", "application/vnd.pinvou.audit-envelope+json")
			_, _ = io.WriteString(w, `{"controlled":"envelope"}`)
		} else {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	defer upstream.Close()
	service.config.AccountURL = parseTestURL(t, upstream.URL)
	id := uuid.NewString()
	for _, tc := range []struct{ method, path string }{
		{"GET", "/admin/v1/audit-logs"}, {"GET", "/admin/v1/audit-logs/" + id},
		{"POST", "/admin/v1/audit-exports"}, {"GET", "/admin/v1/audit-exports/" + id},
		{"POST", "/admin/v1/audit-exports/" + id + "/download-tickets"}, {"GET", "/admin/v1/audit-exports/" + id + "/download"},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			request := financeBrowserRequest(service, session, tc.method, "/api/admin/account"+tc.path, `{}`)
			request.Header.Set("Authorization", "Bearer browser-untrusted")
			request.Header.Set("X-Pinvou-Download-Token", "controlled-one-use-proof")
			response := newAuditDeadlineRecorder()
			service.Handler(nil).ServeHTTP(response, request)
			if response.Code != 200 {
				t.Fatal("actual BFF route rejected", response.Code)
			}
			select {
			case actual := <-requests:
				if actual.method != tc.method || actual.path != tc.path || actual.authorization != "Bearer "+session.AccessToken || actual.cookie != "" || actual.csrf != "" {
					t.Fatal("actual URI or session-only authority changed")
				}
				expected := ""
				if auditExportDownloadPath(tc.method, tc.path) {
					expected = "controlled-one-use-proof"
				}
				if actual.download != expected {
					t.Fatal("download token missing or crossed another route")
				}
			default:
				t.Fatal("BFF did not forward exact Account path")
			}
		})
	}
}
