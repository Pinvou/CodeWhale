package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

type requestIDContextKey struct{}
type traceIDContextKey struct{}

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

func ValidRequestID(value string) bool { return requestIDPattern.MatchString(value) }

func NewCorrelationID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "correlation-id-unavailable"
	}
	return hex.EncodeToString(value[:])
}

func WithCorrelation(ctx context.Context, requestID, traceID string) context.Context {
	if !ValidRequestID(requestID) {
		requestID = NewCorrelationID()
	}
	if strings.TrimSpace(traceID) == "" || len(traceID) > 64 {
		traceID = NewCorrelationID()
	}
	ctx = context.WithValue(ctx, requestIDContextKey{}, requestID)
	return context.WithValue(ctx, traceIDContextKey{}, traceID)
}

func RequestID(ctx context.Context) string {
	value, _ := ctx.Value(requestIDContextKey{}).(string)
	return value
}

func TraceID(ctx context.Context) string {
	if traceID := trace.SpanContextFromContext(ctx).TraceID(); traceID.IsValid() {
		return traceID.String()
	}
	value, _ := ctx.Value(traceIDContextKey{}).(string)
	return value
}

func ContextForTrace(ctx context.Context, requestID, traceID string) context.Context {
	ctx = WithCorrelation(ctx, requestID, traceID)
	parsed, err := trace.TraceIDFromHex(traceID)
	if err != nil || !parsed.IsValid() {
		return ctx
	}
	spanIDBytes := [8]byte{}
	if _, err := rand.Read(spanIDBytes[:]); err != nil {
		return ctx
	}
	spanID := trace.SpanID(spanIDBytes)
	spanContext := trace.NewSpanContext(trace.SpanContextConfig{TraceID: parsed, SpanID: spanID, TraceFlags: trace.FlagsSampled, Remote: true})
	return trace.ContextWithRemoteSpanContext(ctx, spanContext)
}

func StartSpan(ctx context.Context, name string) (context.Context, trace.Span) {
	return otel.Tracer("github.com/Pinvou/pinvou3/account-platform").Start(ctx, name)
}

func TraceHTTPHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(request.Context(), propagation.HeaderCarrier(request.Header))
		ctx, span := otel.Tracer("github.com/Pinvou/pinvou3/account-platform").Start(ctx, "http.server", trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()
		next.ServeHTTP(w, request.WithContext(ctx))
	})
}

func RunBackground(ctx context.Context, task string, run func(context.Context)) {
	ctx = WithCorrelation(ctx, NewCorrelationID(), NewCorrelationID())
	ctx, span := StartSpan(ctx, "background."+task)
	defer span.End()
	slogLogger := slog.Default()
	slogLogger.InfoContext(ctx, "background task started", "task", task)
	defer slogLogger.InfoContext(ctx, "background task stopped", "task", task)
	run(ctx)
}
