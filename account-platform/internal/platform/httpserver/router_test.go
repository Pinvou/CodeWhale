package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
)

func TestAdaptMapsGinParametersAndStableRoutePattern(t *testing.T) {
	router := NewRouter()
	router.GET("/v1/items/:item_id/files/*resource", AdaptFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.PathValue("item_id"); got != "42" {
			t.Fatalf("item_id = %q, want 42", got)
		}
		if got := r.PathValue("resource"); got != "reports/today.json" {
			t.Fatalf("resource = %q, want reports/today.json", got)
		}
		if got := r.Pattern; got != "GET /v1/items/{item_id}/files/{resource...}" {
			t.Fatalf("pattern = %q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/items/42/files/reports/today.json", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
}

func TestRouterReturnsMethodNotAllowed(t *testing.T) {
	router := NewRouter()
	router.GET("/v1/items", AdaptFunc(func(http.ResponseWriter, *http.Request) {}))

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/items", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
}

func TestRequirePathSuffixRejectsColonActionAliases(t *testing.T) {
	for _, endpoint := range []struct{ prefix, route, suffix string }{
		{"/v1", "/organizations/:organization_id/quota-grants:batch", "/quota-grants:batch"},
		{"/v1", "/organizations/:organization_id/account-closed-grants:reclaim-batch", "/account-closed-grants:reclaim-batch"},
		{"", "/internal/v1/organizations/:organization_id/quota-grants:batch", "/quota-grants:batch"},
		{"/internal/v1", "/ledger-repair/authorization-proofs:batch", "/authorization-proofs:batch"},
		{"/admin/v1", "/commands:precheck", "/commands:precheck"},
	} {
		t.Run(endpoint.prefix+endpoint.route, func(t *testing.T) {
			router := NewRouter()
			calls := 0
			router.Group(endpoint.prefix).POST(endpoint.route, RequirePathSuffix(endpoint.suffix), AdaptFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if strings.Contains(endpoint.route, ":organization_id") && r.PathValue("organization_id") != "owned-org" {
					t.Fatal("route guard changed the organization parameter")
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			canonical := endpoint.prefix + strings.ReplaceAll(endpoint.route, ":organization_id", "owned-org")
			for _, probe := range []struct {
				path string
				want int
			}{
				{canonical, http.StatusNoContent},
				{canonical + "?ignored=other", http.StatusNoContent},
				{strings.ReplaceAll(canonical, ":", "%3A"), http.StatusNoContent},
				{canonical + "Other", http.StatusNotFound},
				{strings.ReplaceAll(canonical, ":", ""), http.StatusNotFound},
				{canonical + "/trailing", http.StatusNotFound},
				{canonical + "Other?path=" + endpoint.suffix, http.StatusNotFound},
				{canonical[:strings.LastIndex(canonical, ":")+1] + strings.ToUpper(strings.Split(endpoint.suffix, ":")[1]), http.StatusNotFound},
			} {
				before := calls
				response := httptest.NewRecorder()
				router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, probe.path, nil))
				wantCalls := 0
				if probe.want == http.StatusNoContent {
					wantCalls = 1
				}
				if response.Code != probe.want || calls-before != wantCalls {
					t.Fatalf("%s status=%d calls=%d, want status=%d calls=%d", probe.path, response.Code, calls-before, probe.want, wantCalls)
				}
			}
		})
	}
}

func TestRouterRecoversPanicsAsJSON(t *testing.T) {
	router := NewRouter()
	router.GET("/panic", AdaptFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/panic", nil))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusInternalServerError)
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["code"] != "INTERNAL_ERROR" {
		t.Fatalf("unexpected response: %#v", body)
	}
}

func TestRouterAbortsPanicsAfterResponseStarted(t *testing.T) {
	for _, body := range []string{`{"partial":true}`, "stream chunk\n", "\x00\xffbinary"} {
		t.Run(body, func(t *testing.T) {
			router := NewRouter()
			router.GET("/partial", AdaptFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
				panic("private panic value")
			}))
			response := httptest.NewRecorder()
			defer func() {
				if got := recover(); got != http.ErrAbortHandler {
					t.Errorf("panic = %v, want ErrAbortHandler", got)
				}
				if got := response.Body.String(); got != body {
					t.Errorf("response was changed after panic: %q", got)
				}
			}()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/partial", nil))
		})
	}
}

func TestRouterPreservesExplicitAbortHandler(t *testing.T) {
	router := NewRouter()
	router.GET("/abort", AdaptFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	response := httptest.NewRecorder()
	defer func() {
		if got := recover(); got != http.ErrAbortHandler {
			t.Errorf("panic = %v, want ErrAbortHandler", got)
		}
		if response.Body.Len() != 0 {
			t.Errorf("abort wrote an error response: %q", response.Body.String())
		}
	}()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/abort", nil))
}

func TestRouterAbortsBrokenConnectionsAfterResponseStarted(t *testing.T) {
	for _, connectionError := range []error{syscall.EPIPE, syscall.ECONNRESET} {
		t.Run(connectionError.Error(), func(t *testing.T) {
			router := NewRouter()
			router.GET("/broken", AdaptFunc(func(w http.ResponseWriter, r *http.Request) {
				w.(http.Flusher).Flush()
				panic(connectionError)
			}))
			response := httptest.NewRecorder()
			defer func() {
				if got := recover(); got != http.ErrAbortHandler {
					t.Errorf("panic = %v, want ErrAbortHandler", got)
				}
				if response.Body.Len() != 0 {
					t.Errorf("broken connection wrote an error body: %q", response.Body.String())
				}
			}()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/broken", nil))
		})
	}
}
