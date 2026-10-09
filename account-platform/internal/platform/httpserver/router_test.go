package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
