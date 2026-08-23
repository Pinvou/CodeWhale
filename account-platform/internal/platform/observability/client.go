package observability

import (
	"net/http"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: NewHTTPTransport(nil)}
}

func NewHTTPTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return correlationTransport{next: base}
}

type correlationTransport struct{ next http.RoundTripper }

func (transport correlationTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	ctx, span := otel.Tracer("github.com/Pinvou/pinvou3/account-platform").Start(request.Context(), "http.client", trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()
	clone := request.Clone(ctx)
	if requestID := RequestID(request.Context()); ValidRequestID(requestID) {
		clone.Header.Set("X-Request-ID", requestID)
	}
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(clone.Header))
	response, err := transport.next.RoundTrip(clone)
	if err != nil {
		span.SetStatus(codes.Error, "HTTP_CLIENT_FAILED")
	}
	return response, err
}
