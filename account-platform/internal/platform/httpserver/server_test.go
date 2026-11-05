package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestRequestIDRejectsMalformedOrOversizedExternalValues(t *testing.T) {
	for _, external := range []string{"contains spaces", strings.Repeat("a", 65), "token\r\ninjected: value"} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/health/live", nil)
		request.Header.Set("X-Request-ID", external)
		Handler(config.Config{ServiceName: "test"}).ServeHTTP(response, request)
		requestID := response.Header().Get("X-Request-ID")
		if requestID == external || len(requestID) != 32 {
			t.Fatalf("external request ID %q was trusted as %q", external, requestID)
		}
	}
}

func TestTraceIDIsAcceptedOnlyFromValidTraceparent(t *testing.T) {
	for _, item := range []struct {
		name       string
		parent     string
		expectedID string
	}{
		{name: "valid", parent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", expectedID: "4bf92f3577b34da6a3ce929d0e0e4736"},
		{name: "invalid", parent: "00-not-a-trace-id-00f067aa0ba902b7-01"},
	} {
		t.Run(item.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/health/live", nil)
			request.Header.Set("traceparent", item.parent)
			Handler(config.Config{ServiceName: "test"}).ServeHTTP(response, request)
			traceID := response.Header().Get("X-Trace-ID")
			if item.expectedID != "" && traceID != item.expectedID {
				t.Fatalf("trace ID = %q, want %q", traceID, item.expectedID)
			}
			if item.expectedID == "" && len(traceID) != 32 {
				t.Fatalf("generated trace ID length = %d, want 32", len(traceID))
			}
		})
	}
}

func TestMetricsUseRouteTemplatesWithoutRequestIdentifiers(t *testing.T) {
	handler, registry := handlerWithMetrics(config.Config{ServiceName: "test-service"}, nil, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health/live", nil))
	metrics := httptest.NewRecorder()
	metricsRequest := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsRequest.Header.Set("Accept", "application/openmetrics-text")
	registry.handler().ServeHTTP(metrics, metricsRequest)
	body := metrics.Body.String()
	if !strings.Contains(body, `pinvou_http_requests_total{method="GET",route="GET /health/live",service="test-service",status="200"} 1`) {
		t.Fatalf("route-safe request metric not found:\n%s", body)
	}
	if strings.Contains(body, "request_id=") {
		t.Fatal("request identifier leaked into metrics")
	}
	if !strings.Contains(body, `# {trace_id="`) {
		t.Fatal("trace exemplar is missing from metrics")
	}
}

func TestMetricsAreNotMountedOnTheApplicationListener(t *testing.T) {
	response := httptest.NewRecorder()
	Handler(config.Config{ServiceName: "test"}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("application listener exposed metrics with status %d", response.Code)
	}
}

func TestProductionSecurityHeadersAreFailClosed(t *testing.T) {
	response := httptest.NewRecorder()
	Handler(config.Config{ServiceName: "test", Environment: "production-new"}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health/live", nil))
	for header, expected := range map[string]string{
		"X-Content-Type-Options":    "nosniff",
		"X-Frame-Options":           "DENY",
		"Referrer-Policy":           "no-referrer",
		"Strict-Transport-Security": "max-age=31536000; includeSubDomains",
		"Content-Security-Policy":   "default-src 'none'; frame-ancestors 'none'; base-uri 'none'",
	} {
		if response.Header().Get(header) != expected {
			t.Fatalf("%s = %q, want %q", header, response.Header().Get(header), expected)
		}
	}
}
