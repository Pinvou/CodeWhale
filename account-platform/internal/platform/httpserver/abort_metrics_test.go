package httpserver

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
)

func TestAbortedHTTPResponseIsNotCountedAsSuccessful(t *testing.T) {
	router := NewRouter()
	router.GET("/abort", AdaptFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "6")
		w.WriteHeader(200)
		io.WriteString(w, "abc")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Error(err)
		}
		panic(fmt.Errorf("private-panic-material: %w", http.ErrAbortHandler))
	}))
	router.GET("/normal", AdaptFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	router.GET("/abort500", AdaptFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "6")
		w.WriteHeader(500)
		io.WriteString(w, "abc")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Error(err)
		}
		panic(fmt.Errorf("private-panic-material: %w", http.ErrAbortHandler))
	}))
	router.GET("/normal500", AdaptFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500); io.WriteString(w, "error") }))
	metrics := newHTTPMetrics("fixture")
	var logs bytes.Buffer
	logger := observability.NewLogger(&logs, observability.Config{Service: "fixture"})
	finished := make(chan struct{}, 1)
	handler := metrics.middleware(withRecovery(router), logger)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { finished <- struct{}{} }()
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(server.URL + "/abort")
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || string(body) != "abc" || readErr == nil {
		t.Fatalf("status=%d body=%q err=%v", response.StatusCode, body, readErr)
	}
	<-finished
	response, err = client.Get(server.URL + "/normal")
	if err != nil {
		t.Fatal(err)
	}
	body, readErr = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || string(body) != "ok" || readErr != nil {
		t.Fatal("normal response changed")
	}
	<-finished
	response, err = client.Get(server.URL + "/abort500")
	if err != nil {
		t.Fatal(err)
	}
	body, readErr = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 500 || string(body) != "abc" || readErr == nil {
		t.Fatalf("abort500 status=%d body=%q err=%v", response.StatusCode, body, readErr)
	}
	<-finished
	response, err = client.Get(server.URL + "/normal500")
	if err != nil {
		t.Fatal(err)
	}
	body, readErr = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 500 || string(body) != "error" || readErr != nil {
		t.Fatal("normal 500 response changed")
	}
	<-finished
	families, err := metrics.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	counts, abortCounts := map[string]float64{}, map[string]float64{}
	fiveHundred := regexp.MustCompile(`^5..$`)
	var alertNumerator float64
	for _, family := range families {
		if family.GetName() != "pinvou_http_requests_total" && family.GetName() != "pinvou_http_response_aborts_total" {
			continue
		}
		for _, metric := range family.Metric {
			labels := map[string]string{}
			for _, l := range metric.Label {
				labels[l.GetName()] = l.GetValue()
			}
			if family.GetName() == "pinvou_http_response_aborts_total" {
				abortCounts[labels["route"]] = metric.GetCounter().GetValue()
				continue
			}
			counts[labels["route"]+":"+labels["status"]] = metric.GetCounter().GetValue()
			if fiveHundred.MatchString(labels["status"]) {
				alertNumerator += metric.GetCounter().GetValue()
			}
		}
	}
	if counts["GET /abort:aborted"] != 1 || counts["GET /abort:200"] != 0 || counts["GET /normal:200"] != 1 || counts["GET /abort500:500"] != 1 || counts["GET /normal500:500"] != 1 || abortCounts["GET /abort"] != 1 || abortCounts["GET /abort500"] != 1 || abortCounts["GET /normal"] != 0 || abortCounts["GET /normal500"] != 0 {
		t.Fatalf("unexpected counts %v aborts %v", counts, abortCounts)
	}
	rules, err := os.ReadFile("../../../deploy/helm/account-platform/templates/prometheus-rules.yaml")
	if err != nil || !strings.Contains(string(rules), `rate(pinvou_http_requests_total{status=~"5.."}[5m])`) || alertNumerator != 2 {
		t.Fatalf("existing 5xx numerator missing or incorrect: count=%v err=%v", alertNumerator, err)
	}
	text := logs.String()
	if !strings.Contains(text, `"result":"failure"`) || !strings.Contains(text, `"error_code":"HTTP_RESPONSE_ABORTED"`) || strings.Contains(text, "private-panic-material") {
		t.Fatalf("unsafe or misleading logs: %s", text)
	}
}
