package observability

import (
	"context"
	"testing"
)

func TestProductionTracingRequiresSecureCollector(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	if _, err := SetupTracing(context.Background(), Config{Service: "test", Environment: "production-new"}); err == nil {
		t.Fatal("production accepted a missing OTLP collector")
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318")
	if _, err := SetupTracing(context.Background(), Config{Service: "test", Environment: "production-new"}); err == nil {
		t.Fatal("production accepted plaintext OTLP export")
	}
}

func TestDevelopmentTracingMayBeDisabled(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	shutdown, err := SetupTracing(context.Background(), Config{Service: "test", Environment: "development"})
	if err != nil || shutdown == nil || shutdown(context.Background()) != nil {
		t.Fatalf("disabled development tracing failed: %v", err)
	}
}
