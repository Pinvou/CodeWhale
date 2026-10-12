package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
)

func TestOrdinaryRelayHealthAndMetricsDoNotAdvertiseFutureExecution(t *testing.T) {
	metrics := newRelayMetrics("account")
	metrics.record(events.OutboxStats{Pending: 3, DeadLettered: 1, OldestPendingAge: 2 * time.Second})
	metrics.recordCycle(events.RelayResult{Published: 2}, nil, time.Second)
	server := metrics.server(":0", func(context.Context) error { return errors.New("controlled dependency failure") })
	for _, tc := range []struct {
		path   string
		status int
	}{{"/health/live", 204}, {"/health/ready", 503}, {"/health/ready/approval-authorization", 404}} {
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if response.Code != tc.status {
			t.Fatalf("%s status %d", tc.path, response.Code)
		}
		if strings.Contains(response.Body.String(), "controlled dependency") {
			t.Fatal("dependency error leaked")
		}
	}
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	text := response.Body.String()
	for _, want := range []string{`pinvou_outbox_pending_events{source="account"} 3`, `pinvou_relay_events_total{outcome="published",source="account"} 2`} {
		if !strings.Contains(text, want) {
			t.Fatal("ordinary metric missing")
		}
	}
	if strings.Contains(text, "pinvou_authorization_") {
		t.Fatal("future authorization metrics advertised")
	}
	ready := metrics.server(":0", func(context.Context) error { return nil })
	response = httptest.NewRecorder()
	ready.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if response.Code != 200 {
		t.Fatal("healthy dependencies not ready")
	}
}

func TestOrdinaryRelayRejectsOtherSourcesBeforeDependencies(t *testing.T) {
	for _, source := range []string{"billing", "token", "ACCOUNT"} {
		err := run(context.Background(), func(key string) string {
			if key == "EVENT_SOURCE" {
				return source
			}
			t.Fatal("other source accessed dependencies")
			return ""
		}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err == nil || err.Error() != "ACCOUNT_EVENT_SOURCE_REQUIRED" {
			t.Fatal("other source accepted")
		}
	}
}
