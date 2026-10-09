package observability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestHTTPClientPropagatesW3CTraceContext(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	spanContext := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled})
	ctx := trace.ContextWithSpanContext(context.Background(), spanContext)
	received := ""
	receivedRequestID := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.Header.Get("traceparent")
		receivedRequestID = r.Header.Get("X-Request-ID")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	ctx = WithCorrelation(ctx, "request-123", traceID.String())
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	response, err := NewHTTPClient(0).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if !strings.Contains(received, traceID.String()) {
		t.Fatalf("traceparent %q does not contain parent trace ID", received)
	}
	if receivedRequestID != "request-123" {
		t.Fatalf("request ID = %q, want propagated value", receivedRequestID)
	}
}

func TestHTTPTraceDoesNotRecordURLQueryOrBody(t *testing.T) {
	const marker = "secret-query-and-body-marker"
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider()
	provider.RegisterSpanProcessor(recorder)
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	defer otel.SetTracerProvider(previous)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	request, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL+"/resource?token="+marker, strings.NewReader(marker))
	response, err := NewHTTPClient(0).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	for _, span := range recorder.Ended() {
		if strings.Contains(span.Name(), marker) || strings.Contains(span.Status().Description, marker) {
			t.Fatalf("protected marker leaked into trace metadata: %#v", span)
		}
		for _, attribute := range span.Attributes() {
			if strings.Contains(attribute.Value.Emit(), marker) {
				t.Fatalf("protected marker leaked into trace attribute %q", attribute.Key)
			}
		}
	}
}
