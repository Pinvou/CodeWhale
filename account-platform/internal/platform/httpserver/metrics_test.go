package httpserver

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestMetricsUnknownMethodsShareBoundedSeriesWithoutChangingRequests(t *testing.T) {
	metrics := newHTTPMetrics("test-service")
	var observed []string
	handler := metrics.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed = append(observed, r.Method)
		w.WriteHeader(http.StatusMethodNotAllowed)
	}), slog.New(slog.NewTextHandler(io.Discard, nil)))
	methods := []string{http.MethodGet, http.MethodPost}
	for i := range 100 {
		methods = append(methods, "CUSTOM_"+strconv.Itoa(i))
	}
	for _, method := range methods {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(method, "/same-unmatched-route", nil))
		if response.Code != http.StatusMethodNotAllowed {
			t.Fatal("metric normalization changed application response")
		}
	}
	if len(observed) != len(methods) {
		t.Fatal("metric normalization intercepted requests")
	}
	for i, method := range methods {
		if observed[i] != method {
			t.Fatal("metric normalization changed application method")
		}
	}
	families, err := metrics.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, family := range families {
		if family.GetName() != "pinvou_http_requests_total" && family.GetName() != "pinvou_http_request_duration_seconds" {
			continue
		}
		checked++
		if len(family.Metric) != 3 {
			t.Fatalf("%s series=%d want=3", family.GetName(), len(family.Metric))
		}
		counts := map[string]float64{}
		for _, metric := range family.Metric {
			method := ""
			for _, label := range metric.Label {
				if label.GetName() == "method" {
					method = label.GetValue()
				}
			}
			if metric.Counter != nil {
				counts[method] = metric.Counter.GetValue()
			} else {
				counts[method] = float64(metric.Histogram.GetSampleCount())
			}
		}
		if counts[http.MethodGet] != 1 || counts[http.MethodPost] != 1 || counts["OTHER"] != 100 {
			t.Fatal("standard or other method samples changed")
		}
	}
	if checked != 2 {
		t.Fatal("did not gather both request counter and duration histogram")
	}
}
