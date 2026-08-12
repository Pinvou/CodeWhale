package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
)

func TestHealthEndpoints(t *testing.T) {
	handler := Handler(config.Config{ServiceName: "billing-service"})
	for _, test := range []struct {
		path   string
		status string
	}{
		{path: "/health/live", status: "ok"},
		{path: "/health/ready", status: "ready"},
	} {
		t.Run(test.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, test.path, nil)
			req.Header.Set("X-Request-ID", "test-request")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)

			if response.Code != http.StatusOK {
				t.Fatalf("status code = %d, want 200", response.Code)
			}
			if response.Header().Get("X-Request-ID") != "test-request" {
				t.Fatalf("request id was not preserved")
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", response.Header().Get("Cache-Control"))
			}

			var body statusResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("invalid JSON response: %v", err)
			}
			if body.Service != "billing-service" || body.Status != test.status {
				t.Fatalf("unexpected response: %+v", body)
			}
		})
	}
}

func TestReadinessFailureIsUnavailable(t *testing.T) {
	handler := HandlerWithApplication(
		config.Config{ServiceName: "test"},
		nil,
		func(context.Context) error { return errors.New("database unavailable") },
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status code = %d, want 503", response.Code)
	}
}

func TestApplicationHandlerIsMounted(t *testing.T) {
	application := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	response := httptest.NewRecorder()
	HandlerWithApplication(config.Config{ServiceName: "test"}, application, nil).ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "/v1/example", nil),
	)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status code = %d, want 204", response.Code)
	}
}

func TestUnknownRouteIsNotFound(t *testing.T) {
	response := httptest.NewRecorder()
	Handler(config.Config{ServiceName: "test"}).ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "/missing", nil),
	)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status code = %d, want 404", response.Code)
	}
}

func TestRequestIDIsGeneratedWhenMissing(t *testing.T) {
	response := httptest.NewRecorder()
	Handler(config.Config{ServiceName: "test"}).ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "/health/live", nil),
	)
	if requestID := response.Header().Get("X-Request-ID"); len(requestID) != 32 {
		t.Fatalf("generated request id length = %d, want 32", len(requestID))
	}
}
