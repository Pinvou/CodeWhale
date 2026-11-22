package observability

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	platformconfig "github.com/Pinvou/pinvou3/account-platform/internal/platform/config"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type Config struct {
	Service     string
	Environment string
	Version     string
	Commit      string
}

type Shutdown func(context.Context) error

func SetupTracing(ctx context.Context, config Config) (Shutdown, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	endpoint := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	production := platformconfig.IsProduction(config.Environment)
	if endpoint == "" {
		if production {
			return nil, errors.New("OTEL_EXPORTER_OTLP_ENDPOINT is required in production")
		}
		return func(context.Context) error { return nil }, nil
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, errors.New("OTEL_EXPORTER_OTLP_ENDPOINT must be an HTTP(S) URL without credentials, query, or fragment")
	}
	if production && parsed.Scheme != "https" {
		return nil, errors.New("production OTLP export requires HTTPS")
	}
	options := []otlptracehttp.Option{otlptracehttp.WithEndpointURL(endpoint)}
	if parsed.Scheme == "http" {
		options = append(options, otlptracehttp.WithInsecure())
	}
	exporter, err := otlptracehttp.New(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("create OTLP trace exporter: %w", err)
	}
	serviceResource, err := resource.Merge(resource.Default(), resource.NewWithAttributes("",
		attribute.String("service.name", config.Service),
		attribute.String("service.version", config.Version),
		attribute.String("service.instance.commit", config.Commit),
		attribute.String("deployment.environment.name", config.Environment),
	))
	if err != nil {
		return nil, fmt.Errorf("create OpenTelemetry resource: %w", err)
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(serviceResource),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
	)
	otel.SetTracerProvider(provider)
	return provider.Shutdown, nil
}
