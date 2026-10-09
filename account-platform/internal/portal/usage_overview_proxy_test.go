package portal

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

func TestF05OverviewSessionFlags(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	session := validSession(uuid.NewString())
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	for name := range map[string]bool{"AdminModelOperationsOverviewEnabled": true, "AdminModelTokenPageOverviewsEnabled": true} {
		field := reflect.ValueOf(&service.config).Elem().FieldByName(name)
		if !field.IsValid() {
			t.Fatalf("missing original flag %s", name)
		}
		field.SetBool(true)
	}
	request := financeBrowserRequest(service, session, http.MethodGet, "/api/session", "")
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	var payload struct {
		Authenticated bool            `json:"authenticated"`
		FeatureFlags  map[string]bool `json:"feature_flags"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &payload) != nil || !payload.Authenticated || !payload.FeatureFlags["admin_model_operations_overview"] || !payload.FeatureFlags["admin_model_token_page_overviews"] {
		t.Fatal("authenticated flag projection unavailable")
	}
	request.Header.Del("Cookie")
	response = httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	if response.Code != 401 {
		t.Fatalf("anonymous status %d", response.Code)
	}
}

func TestF05OverviewProxyExactReadAuthority(t *testing.T) {
	for _, tc := range []struct {
		name, path, query string
		status            int
	}{
		{"aggregate", "/admin/v1/operations-overview", "window=7d", 200},
		{"token-only", "/admin/v1/overviews/model-operations", "", 200},
		{"usage-only", "/admin/v1/overviews/model-operations", "window=24h", 200},
		{"both", "/admin/v1/overviews/model-operations", "window=30d", 200},
		{"owner-forbidden", "/admin/v1/overviews/model-operations", "window=24h", 403},
		{"owner-unavailable", "/admin/v1/operations-overview", "window=24h", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemoryStore()
			service := testService(t, store)
			session := validSession(uuid.NewString())
			if err := store.SaveSession(context.Background(), session); err != nil {
				t.Fatal(err)
			}
			raw := `{"counter":9007199254740993,"code":"CONTROLLED_OWNER"}`
			var calls atomic.Int32
			owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "GET" || r.URL.Path != tc.path || r.URL.RawQuery != tc.query || r.Header.Get("Authorization") != "Bearer "+session.AccessToken || r.Header.Get("Cookie") != "" || r.Header.Get("X-Pinvou-Permissions") != "" {
					t.Error("wrong read lineage or authority")
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "3")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, raw)
			}))
			defer owner.Close()
			service.config.TokenAdminProxyHMACKey = []byte("controlled-f05-synthetic-proxy-key-32")
			service.config.TokenGatewayURL = parseTestURL(t, owner.URL)
			path := "/api/admin/token" + tc.path
			if tc.query != "" {
				path += "?" + tc.query
			}
			request := financeBrowserRequest(service, session, "GET", path, "")
			request.Header.Set("Authorization", "Bearer browser-forged")
			request.Header.Set("X-Pinvou-Permissions", "platform.usage.read")
			response := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(response, request)
			if response.Code != tc.status || response.Body.String() != raw || calls.Load() != 1 || response.Header().Get("Retry-After") != "3" {
				t.Fatalf("read result status=%d calls=%d", response.Code, calls.Load())
			}
			request.Header.Del("Cookie")
			response = httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(response, request)
			if response.Code != 401 || calls.Load() != 1 {
				t.Fatal("guest reached owner")
			}
		})
	}
}

func TestF05OverviewWhitelistRemainsReadOnlyAndClosed(t *testing.T) {
	for _, tc := range []struct{ method, path string }{{"POST", "/admin/v1/operations-overview"}, {"PUT", "/admin/v1/overviews/model-operations"}, {"GET", "/admin/v1/overviews/future"}, {"GET", "/admin/v1/overviews/model-operations/extra"}, {"GET", "/v1/chat/completions"}} {
		if tokenAdminPathAllowed(tc.method, tc.path) {
			t.Fatalf("uninstalled %s %s", tc.method, tc.path)
		}
	}
}
