package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
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
