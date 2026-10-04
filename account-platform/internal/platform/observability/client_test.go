package observability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func TestHTTPClientPropagatesW3CTraceContext(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	spanContext := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled})
	ctx := trace.ContextWithSpanContext(context.Background(), spanContext)
	received := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.Header.Get("traceparent")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	response, err := NewHTTPClient(0).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if !strings.Contains(received, traceID.String()) {
		t.Fatalf("traceparent %q does not contain parent trace ID", received)
	}
}

func TestObservedHTTPClientDoesNotForwardSecretsAcrossRedirect(t *testing.T) {
	forwarded := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { forwarded = true }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(307)
	}))
	defer redirect.Close()
	req, _ := http.NewRequest(http.MethodPost, redirect.URL, strings.NewReader("private body"))
	req.Header.Set("Authorization", "Bearer fixture-only")
	response, err := NewHTTPClient(0).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 307 || forwarded {
		t.Fatalf("status=%d forwarded=%v", response.StatusCode, forwarded)
	}
}
