package portal

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestRetainedF04PageOverviewProxy(t *testing.T) {
	for _, path := range []string{"/admin/v1/overviews/model-catalog", "/admin/v1/overviews/token-resources", "/admin/v1/overviews/model-operations"} {
		t.Run(path, func(t *testing.T) {
			store := newMemoryStore()
			service := testService(t, store)
			service.config.TokenAdminProxyHMACKey = []byte("retained-f04-synthetic-proxy-key-32")
			session := validSession(uuid.NewString())
			if err := store.SaveSession(context.Background(), session); err != nil {
				t.Fatal(err)
			}
			raw := `{"schema_version":"1","as_of":"2026-10-09T00:00:00Z","metrics":[{"value":9007199254740993}]}`
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodGet || r.URL.Path != path || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer "+session.AccessToken || r.Header.Get("Cookie") != "" {
					t.Error("overview authority or resource changed")
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Platform-Authorization-Version", "7")
				_, _ = io.WriteString(w, raw)
			}))
			defer upstream.Close()
			service.config.TokenGatewayURL = parseTestURL(t, upstream.URL)
			request := financeBrowserRequest(service, session, http.MethodGet, "/api/admin/token"+path, "")
			request.Header.Set("Authorization", "Bearer untrusted-browser")
			response := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(response, request)
			if response.Code != 200 || response.Body.String() != raw || response.Header().Get("Cache-Control") != "no-store" || calls.Load() != 1 {
				t.Fatalf("overview status=%d calls=%d body=%s", response.Code, calls.Load(), response.Body.String())
			}
			request = httptest.NewRequest(http.MethodGet, "/api/admin/token"+path, nil)
			response = httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized || calls.Load() != 1 {
				t.Fatalf("missing session crossed authority: %d", response.Code)
			}
		})
	}
}

func TestRetainedF04OverviewSessionFlags(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	if err := json.Unmarshal([]byte(`{"AdminModelOperationsOverviewEnabled":true,"AdminModelTokenPageOverviewsEnabled":true}`), &service.config); err != nil {
		t.Fatal(err)
	}
	session := validSession(uuid.NewString())
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	request := financeBrowserRequest(service, session, http.MethodGet, "/api/session", "")
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	var payload struct {
		Authenticated bool            `json:"authenticated"`
		Flags         map[string]bool `json:"feature_flags"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || !payload.Authenticated || !payload.Flags["admin_model_operations_overview"] || !payload.Flags["admin_model_token_page_overviews"] {
		t.Fatal("configured overview flags did not reach real session response")
	}
}

func TestRetainedF04OverviewFutureAndMethodDeny(t *testing.T) {
	for _, path := range []string{"/admin/v1/operations-overview", "/admin/v1/usage-requests", "/admin/v1/reconciliation-cases", "/admin/v1/overviews/unknown", "/admin/v1/overviews/model-catalog/extra"} {
		if tokenAdminPathAllowed(http.MethodGet, path) {
			t.Fatalf("future or malformed path allowed %s", path)
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		if tokenAdminPathAllowed(method, "/admin/v1/overviews/model-catalog") {
			t.Fatalf("overview mutation allowed %s", method)
		}
	}
}
