package portal

import (
	"context"
	"github.com/google/uuid"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestManualGrantRecoveryProxyAllowlist(t *testing.T) {
	const base = "/admin/v1/approval-grant-recovery-cases"
	const id = "8e1892ad-ea8e-4c23-a2f7-bcc2d193ac17"
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		for _, path := range []string{base, base + "/" + id} {
			if !accountAdminPathAllowed(method, path) {
				t.Fatal("read not routed", method, path)
			}
		}
	}
	for _, action := range []string{"observations", "proposals", "reviews"} {
		if !accountAdminPathAllowed(http.MethodPost, base+"/"+id+"/"+action) {
			t.Fatal("manual action not routed", action)
		}
	}
	for _, pair := range [][2]string{{"POST", base}, {"DELETE", base + "/" + id}, {"POST", base + "/" + id + "/execute"}, {"GET", base + "/" + id + "/reviews"}, {"POST", base + "/not-an-id/reviews"}, {"POST", base + "/" + id + "/reviews/extra"}} {
		if accountAdminPathAllowed(pair[0], pair[1]) {
			t.Fatal("unregistered operation exposed", pair)
		}
	}
}

func TestManualGrantRecoveryPortalPreservesSessionAndBody(t *testing.T) {
	for _, tc := range []struct {
		method, suffix string
		csrf           bool
		want           int
	}{
		{"GET", "", true, 200}, {"GET", "/8e1892ad-ea8e-4c23-a2f7-bcc2d193ac17", true, 200},
		{"POST", "/8e1892ad-ea8e-4c23-a2f7-bcc2d193ac17/observations", true, 200},
		{"POST", "/8e1892ad-ea8e-4c23-a2f7-bcc2d193ac17/proposals", true, 200},
		{"POST", "/8e1892ad-ea8e-4c23-a2f7-bcc2d193ac17/reviews", true, 200},
		{"POST", "/8e1892ad-ea8e-4c23-a2f7-bcc2d193ac17/reviews", false, 403},
	} {
		t.Run(tc.method+tc.suffix+manualGrantRecoveryCSRFSuffix(tc.csrf), func(t *testing.T) {
			store := newMemoryStore()
			service := testService(t, store)
			session := validSession(uuid.NewString())
			if err := store.SaveSession(context.Background(), session); err != nil {
				t.Fatal(err)
			}
			body := `{"operation_id":"8e1892ad-ea8e-4c23-a2f7-bcc2d193ac17","expected_version":2,"evidence":{"reference":"CONTROLLED-1"}}`
			if tc.method == http.MethodGet {
				body = ""
			}
			var hits atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				raw, _ := io.ReadAll(r.Body)
				if r.URL.Path != "/admin/v1/approval-grant-recovery-cases"+tc.suffix || string(raw) != body || r.Header.Get("Authorization") != "Bearer "+session.AccessToken || r.Header.Get("Cookie") != "" || r.Header.Get("X-CSRF-Token") != "" {
					t.Error("manual recovery proxy changed authority or body")
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				_, _ = io.WriteString(w, `{"controlled_upstream":true}`)
			}))
			defer upstream.Close()
			service.config.AccountURL = parseTestURL(t, upstream.URL)
			req := financeBrowserRequest(service, session, tc.method, "/api/admin/account/admin/v1/approval-grant-recovery-cases"+tc.suffix, body)
			req.Header.Set("Authorization", "Bearer forged-browser")
			if !tc.csrf {
				req.Header.Del("X-CSRF-Token")
			}
			response := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(response, req)
			if response.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", response.Code, tc.want, response.Body.String())
			}
			if tc.want == 200 && (hits.Load() != 1 || !strings.Contains(response.Body.String(), "controlled_upstream")) {
				t.Fatal("manual recovery did not reach Account exactly once")
			}
			if tc.want == 403 && hits.Load() != 0 {
				t.Fatal("CSRF failure reached Account")
			}
		})
	}
}

func manualGrantRecoveryCSRFSuffix(v bool) string {
	if v {
		return "-csrf"
	}
	return "-no-csrf"
}
