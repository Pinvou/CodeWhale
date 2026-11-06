package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestRelayObservabilityEnvironmentAndMode(t *testing.T) {
	original := gin.Mode()
	t.Cleanup(func() { gin.SetMode(original) })
	for _, test := range []struct{ environment, explicit, wantEnvironment, wantMode string }{
		{"", "", "development", gin.DebugMode},
		{" \t", "", "development", gin.DebugMode},
		{"production", "", "production", gin.ReleaseMode},
		{" staging ", "", "staging", gin.ReleaseMode},
		{"production", "debug", "production", gin.DebugMode},
		{"development", "release", "development", gin.ReleaseMode},
		{"test", "", "test", gin.TestMode},
	} {
		t.Run(test.environment+"/"+test.explicit, func(t *testing.T) {
			t.Setenv("GIN_MODE", test.explicit)
			input := func(key string) string {
				if key == "APP_ENV" {
					return test.environment
				}
				return ""
			}
			if _, err := configureRelayLogging(input); err != nil {
				t.Fatal(err)
			}
			if got := relayObservabilityConfig(input).Environment; got != test.wantEnvironment {
				t.Fatalf("environment = %q, want %q", got, test.wantEnvironment)
			}
			if gin.Mode() != test.wantMode {
				t.Fatalf("mode = %q, want %q", gin.Mode(), test.wantMode)
			}
		})
	}
	t.Setenv("GIN_MODE", "invalid")
	prior := gin.Mode()
	if _, err := configureRelayLogging(func(string) string { return "production" }); err == nil {
		t.Fatal("invalid diagnostic mode accepted")
	}
	if gin.Mode() != prior {
		t.Fatal("invalid mode changed process mode")
	}
}

func TestRelayBuildIdentityReachesLoggerAndTracing(t *testing.T) {
	wantVersion, wantCommit := os.Getenv("PINVOU_TEST_RELAY_BUILD_VERSION"), os.Getenv("PINVOU_TEST_RELAY_BUILD_COMMIT")
	if wantVersion == "" {
		wantVersion = "dev"
	}
	if wantCommit == "" {
		wantCommit = "unknown"
	}
	config := relayObservabilityConfig(func(string) string { return "production" })
	var output bytes.Buffer
	observability.NewLogger(&output, config).Info("application event")
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"service": "event-relay", "environment": "production", "version": wantVersion, "commit": wantCommit} {
		if record[key] != want {
			t.Fatalf("logger %s = %v, want %s", key, record[key], want)
		}
	}
	// Inspect the SDK resource without ending/exporting a span. The reserved
	// loopback endpoint is never contacted: shutdown precedes span.End below.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://127.0.0.1:1")
	prior := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prior) })
	shutdown, err := setupRelayTracing(context.Background(), func(string) string { return "production" })
	if err != nil {
		t.Fatal(err)
	}
	_, span := otel.Tracer("relay-build-identity-fixture").Start(context.Background(), "fixture")
	defer func() {
		if err := shutdown(context.Background()); err != nil {
			t.Error(err)
		}
		span.End()
	}()
	readable, ok := span.(sdktrace.ReadOnlySpan)
	if !ok {
		t.Fatal("expected real SDK recording span")
	}
	attributes := map[string]string{}
	for _, attribute := range readable.Resource().Attributes() {
		attributes[string(attribute.Key)] = attribute.Value.AsString()
	}
	for key, want := range map[string]string{"service.name": "event-relay", "deployment.environment.name": "production", "service.version": wantVersion, "service.instance.commit": wantCommit} {
		if attributes[key] != want {
			t.Fatalf("tracing %s = %q, want %q", key, attributes[key], want)
		}
	}
}
