package httpserver

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
)

func TestHealthProbesStayReachableAfterClientRateLimit(t *testing.T) {
	readyCalls := 0
	handler := HandlerWithApplication(config.Config{ServiceName: "fixture", RateLimitRPS: 1, RateLimitBurst: 1},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }),
		func(context.Context) error { readyCalls++; return nil })
	request := func(method, path string) int {
		r := httptest.NewRequest(method, path, nil)
		r.RemoteAddr = "192.0.2.1:1234"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	if request(http.MethodGet, "/business") != http.StatusNoContent || request(http.MethodGet, "/business") != http.StatusTooManyRequests {
		t.Fatal("client budget was not exhausted")
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		for _, path := range []string{"/health/live", "/health/ready"} {
			if got := request(method, path); got != http.StatusOK {
				t.Fatalf("probe %s %s = %d", method, path, got)
			}
		}
	}
	if readyCalls != 2 {
		t.Fatal("rate-limit exception bypassed actual readiness checks")
	}
}

func TestUnknownHTTPMethodsShareOneMetricDimension(t *testing.T) {
	metrics := newHTTPMetrics("fixture")
	handler := metrics.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }), slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, method := range []string{"CUSTOM_A", "CUSTOM_B", http.MethodGet} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, "/", nil))
	}
	families, err := metrics.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "pinvou_http_requests_total" {
			continue
		}
		if len(family.Metric) != 2 {
			t.Fatalf("arbitrary methods created %d metric series", len(family.Metric))
		}
		foundOther := false
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if label.GetName() == "method" && label.GetValue() == "OTHER" {
					foundOther = metric.GetCounter().GetValue() == 2
				}
			}
		}
		if !foundOther {
			t.Fatal("unknown methods were not grouped without changing requests")
		}
		return
	}
	t.Fatal("HTTP request metric missing")
}
