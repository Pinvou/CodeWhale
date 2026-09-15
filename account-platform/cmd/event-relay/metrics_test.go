package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
)

func TestRelayHealthSeparatesLivenessAndDependencyReadiness(t *testing.T) {
	server := newRelayMetrics("account").server(":0", func(context.Context) error { return errors.New("dependency unavailable") })
	live := httptest.NewRecorder()
	server.Handler.ServeHTTP(live, httptest.NewRequest(http.MethodGet, "/health/live", nil))
	if live.Code != http.StatusNoContent {
		t.Fatalf("liveness status = %d", live.Code)
	}
	ready := httptest.NewRecorder()
	server.Handler.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness status = %d", ready.Code)
	}
}

func TestRelayHealthKeepsGenericReadyWhenApprovalChainIsDegraded(t *testing.T) {
	server := newRelayMetrics("account").serverWithApproval(":0", func(context.Context) error { return nil }, func(context.Context) error { return errors.New("approval relay unavailable") })
	generic := httptest.NewRecorder()
	server.Handler.ServeHTTP(generic, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if generic.Code != http.StatusOK {
		t.Fatalf("generic readiness status = %d", generic.Code)
	}
	approval := httptest.NewRecorder()
	server.Handler.ServeHTTP(approval, httptest.NewRequest(http.MethodGet, "/health/ready/approval-authorization", nil))
	if approval.Code != http.StatusServiceUnavailable {
		t.Fatalf("approval readiness status = %d", approval.Code)
	}
}

func TestRelayMetricsKeepGenericAndAuthorizationCyclesSeparate(t *testing.T) {
	metrics := newRelayMetrics("account")
	metrics.recordCycle(events.RelayResult{Published: 2}, nil, time.Second)
	metrics.recordApprovalCycle(events.RelayResult{Failed: 1}, errors.New("closed failure"), 2*time.Second)
	server := metrics.server(":0", func(context.Context) error { return nil })
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body, err := io.ReadAll(response.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, expected := range []string{
		`pinvou_relay_events_total{outcome="published",source="account"} 2`,
		`pinvou_authorization_relay_events_total{outcome="retry",source="account"} 1`,
		`pinvou_relay_cycles_total{outcome="success",source="account"} 1`,
		`pinvou_authorization_relay_cycles_total{outcome="failure",source="account"} 1`,
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing isolated metric %q", expected)
		}
	}
}

func TestAuthorizationRelayHandlerMetricNeverUnderflows(t *testing.T) {
	metrics := newRelayMetrics("account")
	metrics.EndFrozenPublish()
	metrics.BeginFrozenPublish()
	metrics.BeginFrozenPublish()
	metrics.EndFrozenPublish()
	metrics.EndFrozenPublish()
	metrics.EndFrozenPublish()
	if got := metrics.authorizationHandlers.Load(); got != 0 {
		t.Fatalf("active handlers = %d", got)
	}

	server := metrics.server(":0", func(context.Context) error { return nil })
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body, err := io.ReadAll(response.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `pinvou_authorization_relay_active_handlers{source="account"} 0`) {
		t.Fatal("authorization handler gauge is missing or negative")
	}
}
