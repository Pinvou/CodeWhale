package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func diagnosticCode(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err = json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	code, _ := result["code"].(string)
	return code
}

type diagnosticErrorTransport struct{ err error }

func (tr diagnosticErrorTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, tr.err
}

func TestSystemProbeBuiltInDiagnosticCodes(t *testing.T) {
	for _, c := range []struct {
		name                       string
		status                     int
		body, field, code, message string
	}{
		{"http", 503, "", "", "PROBE_HTTP_ERROR", "HTTP 503"},
		{"invalid", 200, "{", "success", "PROBE_RESPONSE_INVALID", "configured status response is invalid"},
		{"assertion", 200, `{"success":false}`, "success", "PROBE_ASSERTION_FAILED", "configured status assertion failed"},
		{"healthy", 200, `{"success":true}`, "success", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(c.status); fmt.Fprint(w, c.body) }))
			defer server.Close()
			service := testService(t, newMemoryStore())
			got := service.probe(context.Background(), SystemProbe{Target: parseTestURL(t, server.URL), JSONBooleanField: c.field})
			if diagnosticCode(t, got) != c.code || got.Message != c.message {
				t.Fatalf("code=%s message=%q", diagnosticCode(t, got), got.Message)
			}
		})
	}
	t.Run("not-configured", func(t *testing.T) {
		got := testService(t, newMemoryStore()).probe(context.Background(), SystemProbe{})
		if diagnosticCode(t, got) != "STATUS_ENDPOINT_NOT_CONFIGURED" || got.Message != "status endpoint is not configured" {
			t.Fatal(got)
		}
	})
	for _, c := range []struct {
		name          string
		err           error
		code, message string
	}{
		{"timeout", context.DeadlineExceeded, "PROBE_REQUEST_TIMED_OUT", "request timed out"},
		{"connection", errors.New("private underlying cause"), "PROBE_CONNECTION_FAILED", "connection failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			service := testService(t, newMemoryStore())
			service.probeClient = &http.Client{Transport: diagnosticErrorTransport{c.err}}
			got := service.probe(context.Background(), SystemProbe{Target: parseTestURL(t, "https://controlled.invalid")})
			if diagnosticCode(t, got) != c.code || got.Message != c.message {
				t.Fatalf("code=%s message=%q", diagnosticCode(t, got), got.Message)
			}
		})
	}
}

func TestSystemRelayBuiltInDiagnosticCodes(t *testing.T) {
	for _, c := range []struct {
		name                string
		health, metrics     int
		body, code, message string
	}{
		{"health", 503, 200, "", "PROBE_HTTP_ERROR", "HTTP 503"},
		{"metrics-http", 204, 503, "", "RELAY_METRICS_HTTP_ERROR", "metrics HTTP 503"},
		{"metrics-invalid", 204, 200, "invalid", "RELAY_METRICS_INVALID", "relay metrics are invalid"},
		{"dead-letter", 204, 200, "pinvou_outbox_pending_events 2\npinvou_outbox_dead_lettered_events 3\npinvou_outbox_oldest_pending_age_seconds 1.25\n", "RELAY_DEAD_LETTERED_EVENTS", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/health/live" {
					w.WriteHeader(c.health)
					return
				}
				w.WriteHeader(c.metrics)
				fmt.Fprint(w, c.body)
			}))
			defer server.Close()
			service := testService(t, newMemoryStore())
			got := service.probeRelay(context.Background(), "controlled-relay", parseTestURL(t, server.URL))
			if diagnosticCode(t, got) != c.code || got.Message != c.message {
				t.Fatalf("code=%s message=%q", diagnosticCode(t, got), got.Message)
			}
			if c.name == "dead-letter" && (got.PendingEvents != 2 || got.DeadLetteredEvents != 3 || got.OldestPendingSecond != 1.25 || got.Status != "DEGRADED") {
				t.Fatal("metric facts changed")
			}
		})
	}
}

func TestSystemBuiltInCodesPropagateToRecentErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/probe":
			fmt.Fprint(w, `{"success":false}`)
		case "/health/live":
			w.WriteHeader(204)
		case "/metrics":
			fmt.Fprint(w, "pinvou_outbox_pending_events 2\npinvou_outbox_dead_lettered_events 3\npinvou_outbox_oldest_pending_age_seconds 1.25\n")
		case "/admin/v1/overview":
			fmt.Fprint(w, `{"reconciliation_pending":0,"reconciliation_stopped":0}`)
		case "/admin/v1/usage-requests":
			fmt.Fprint(w, `{"items":[]}`)
		default:
			t.Errorf("unexpected local route %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	service := testService(t, newMemoryStore())
	service.config.TokenGatewayURL = parseTestURL(t, server.URL)
	service.config.SystemProbes = []SystemProbe{{Key: "service", Target: parseTestURL(t, server.URL+"/probe"), JSONBooleanField: "success"}, {Key: "relay", Kind: "relay", Target: parseTestURL(t, server.URL)}}
	got := service.collectSystemStatus(context.Background(), "")
	codes := map[string]string{}
	for _, item := range got.RecentErrors {
		codes[item.Source] = item.Code
	}
	if len(codes) != 2 || codes["service"] != "PROBE_ASSERTION_FAILED" || codes["relay"] != "RELAY_DEAD_LETTERED_EVENTS" {
		t.Fatal(codes)
	}
	if len(got.Services) != 1 || len(got.Relays) != 1 || got.Relays[0].DeadLetteredEvents != 3 {
		t.Fatal("existing status facts changed")
	}
}
