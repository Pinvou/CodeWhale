package httpserver

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
)

func TestHijackedResponseHasExplicitOutcomeWithoutInventedSuccess(t *testing.T) {
	metrics := newHTTPMetrics("fixture")
	var logs bytes.Buffer
	logger := observability.NewLogger(&logs, observability.Config{Service: "fixture"})
	router := NewRouter()
	router.GET("/handover", AdaptFunc(func(w http.ResponseWriter, _ *http.Request) {
		connection, buffer, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error("real connection handover failed")
			return
		}
		defer connection.Close()
		if _, err := buffer.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"); err != nil {
			t.Error("controlled handover response failed")
			return
		}
		if err := buffer.Flush(); err != nil {
			t.Error("controlled handover flush failed")
		}
	}))
	router.GET("/normal", AdaptFunc(func(w http.ResponseWriter, _ *http.Request) {
		api.WriteJSON(w, http.StatusOK, map[string]bool{"next": true})
	}))
	router.GET("/written-handover", AdaptFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "2")
		w.WriteHeader(http.StatusCreated)
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Error("controlled response header flush failed")
			return
		}
		connection, buffer, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error("handover after headers failed")
			return
		}
		defer connection.Close()
		if _, err := buffer.WriteString("ok"); err != nil {
			t.Error("controlled handed-over body failed")
			return
		}
		if err := buffer.Flush(); err != nil {
			t.Error("controlled handed-over body flush failed")
		}
	}))
	finished := make(chan struct{}, 1)
	handler := metrics.middleware(withRecovery(router), logger)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { finished <- struct{}{} }()
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	for _, path := range []string{"/handover", "/written-handover", "/normal"} {
		response, err := client.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		wantStatus := http.StatusOK
		if path == "/written-handover" {
			wantStatus = http.StatusCreated
		}
		if readErr != nil || response.StatusCode != wantStatus || (path != "/normal" && string(body) != "ok") {
			t.Fatal("connection handover or next ordinary response changed", readErr)
		}
		<-finished
	}
	// A rejected handover must not count as a successful transfer.
	request := httptest.NewRequest(http.MethodGet, "/unsupported", nil)
	request.Pattern = "GET /unsupported"
	metrics.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, _, err := http.NewResponseController(w).Hijack(); err == nil {
			t.Error("recorder unexpectedly supports handover")
		}
		w.WriteHeader(http.StatusBadGateway)
	}), logger).ServeHTTP(httptest.NewRecorder(), request)
	families, err := metrics.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	requests, hijacks := map[string]float64{}, map[string]float64{}
	for _, family := range families {
		for _, metric := range family.Metric {
			labels := map[string]string{}
			for _, label := range metric.Label {
				labels[label.GetName()] = label.GetValue()
			}
			switch family.GetName() {
			case "pinvou_http_requests_total":
				requests[labels["route"]+":"+labels["status"]] = metric.GetCounter().GetValue()
			case "pinvou_http_response_hijacks_total":
				hijacks[labels["route"]] = metric.GetCounter().GetValue()
			}
		}
	}
	if requests["GET /handover:hijacked"] != 1 || requests["GET /handover:200"] != 0 || requests["GET /normal:200"] != 1 || requests["GET /written-handover:201"] != 1 || requests["GET /unsupported:502"] != 1 || hijacks["GET /handover"] != 1 || hijacks["GET /written-handover"] != 1 || hijacks["GET /unsupported"] != 0 {
		t.Fatal("handover outcome or following request was misclassified", requests, hijacks)
	}
	if !strings.Contains(logs.String(), `"error_code":"HTTP_RESPONSE_HIJACKED"`) {
		t.Fatal("connection handover was not observable")
	}
}

func TestMetricsListenerRetainsFiniteReadAndWriteBudgets(t *testing.T) {
	metrics := newHTTPMetrics("fixture")
	server := newMetricsServer("127.0.0.1:0", metrics.handler())
	for name, budget := range map[string]time.Duration{"header": server.ReadHeaderTimeout, "read": server.ReadTimeout, "write": server.WriteTimeout, "idle": server.IdleTimeout} {
		if budget <= 0 || budget > 30*time.Second {
			t.Fatal("metrics listener has an unbounded or excessive I/O budget", name)
		}
	}
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "pinvou_service_readiness") {
		t.Fatal("metrics scrape changed")
	}
}
