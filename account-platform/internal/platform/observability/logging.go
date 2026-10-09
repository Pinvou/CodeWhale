package observability

import (
	"context"
	"io"
	"log"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
)

var safeLogEvents = map[string]string{
	"application event":                    "application_event",
	"approval recovery pass":               "approval_recovery_pass",
	"approval recovery failed":             "approval_recovery_failed",
	"service listening":                    "service_listening",
	"metrics listening":                    "metrics_listening",
	"service shutting down":                "service_shutting_down",
	"http request completed":               "http_request_completed",
	"http handler panic recovered":         "http_handler_panic_recovered",
	"mobile authentication request failed": "mobile_authentication_request_failed",
	"mobile authentication request rejected by registration conflict": "mobile_authentication_registration_conflict",
	"registration operation conflict":                                 "registration_operation_conflict",
	"registration recovery cycle failed":                              "registration_recovery_cycle_failed",
	"wallet provisioning cycle failed":                                "wallet_provisioning_cycle_failed",
	"secret key rotation failed":                                      "secret_key_rotation_failed",
	"secret key rotation batch completed":                             "secret_key_rotation_batch_completed",
	"relay metrics server stopped":                                    "relay_metrics_server_stopped",
	"outbox relay cycle failed":                                       "outbox_relay_cycle_failed",
	"outbox relay cycle completed":                                    "outbox_relay_cycle_completed",
	"outbox metrics query failed":                                     "outbox_metrics_query_failed",
	"claim failed":                                                    "usage_reconciliation_claim_failed",
	"request reconciliation deferred":                                 "usage_reconciliation_deferred",
	"claim expired payment orders failed":                             "expired_payment_claim_failed",
	"escalate late payment review failed":                             "late_payment_review_escalation_failed",
	"reschedule pending payment order failed":                         "pending_payment_reschedule_failed",
	"escalate late payment review after boundary query failed":        "boundary_payment_review_escalation_failed",
	"reschedule expired payment order failed":                         "expired_payment_reschedule_failed",
	"expired payment order reconciliation deferred":                   "expired_payment_reconciliation_deferred",
	"scheduled payment reconciliation failed":                         "scheduled_payment_reconciliation_failed",
	"background task started":                                         "background_task_started",
	"background task stopped":                                         "background_task_stopped",
	"legacy standard log":                                             "legacy_standard_log",
}

var safeLogText = regexp.MustCompile(`^[A-Za-z0-9_./:@{} -]{1,256}$`)

var safeLogFields = map[string]struct{}{
	"service": {}, "environment": {}, "version": {}, "commit": {}, "result": {},
	"source": {}, "component": {}, "task": {}, "request_id": {}, "trace_id": {},
	"method": {}, "route": {}, "status": {}, "duration_ms": {}, "address": {},
	"error_code": {}, "claimed": {}, "published": {}, "failed": {}, "dead_lettered": {},
	"attempt": {}, "rotated": {}, "remaining": {}, "channel": {}, "business_date": {},
	"stage": {}, "operation_id": {}, "operation_status": {}, "operation_version": {},
	"stored_fingerprint": {}, "calculated_fingerprint": {}, "stored_fingerprint_length": {},
	"username_length": {}, "mobile_length": {}, "policy_version": {}, "challenge_id": {},
	"rows_affected": {},
	"candidates":    {}, "confirmed": {}, "invalidated": {}, "pending": {}, "unavailable": {}, "more_candidates": {},
}

// NewLogger returns the only logger schema used by Account Platform services.
// Unknown events and attributes are deliberately collapsed instead of being
// serialized, so error strings, request bodies and credentials cannot leak.
func NewLogger(output io.Writer, config Config) *slog.Logger {
	if output == nil {
		output = io.Discard
	}
	base := slog.NewJSONHandler(output, &slog.HandlerOptions{})
	handler := &safeLogHandler{inner: base}
	return slog.New(handler).With(
		"service", boundedLogText(config.Service, "unknown"),
		"environment", boundedLogText(config.Environment, "unknown"),
		"version", boundedLogText(config.Version, "unknown"),
		"commit", boundedLogText(config.Commit, "unknown"),
	)
}

func ConfigureDefaultLogger(config Config) *slog.Logger {
	logger := NewLogger(os.Stdout, config)
	slog.SetDefault(logger)
	log.SetFlags(0)
	log.SetPrefix("")
	log.SetOutput(&standardLogWriter{logger: logger})
	return logger
}

type standardLogWriter struct {
	logger *slog.Logger
	mu     sync.Mutex
}

func (writer *standardLogWriter) Write(body []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	writer.logger.Error("legacy standard log", "error_code", "LEGACY_STANDARD_LOG")
	return len(body), nil
}

type safeLogHandler struct {
	inner slog.Handler
	attrs []slog.Attr
}

func (handler *safeLogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return handler.inner.Enabled(ctx, level)
}

func (handler *safeLogHandler) Handle(ctx context.Context, record slog.Record) error {
	event, ok := safeLogEvents[strings.ToLower(strings.TrimSpace(record.Message))]
	if !ok {
		event = "application_event"
	}
	result := "success"
	if record.Level >= slog.LevelError {
		result = "failure"
	} else if record.Level >= slog.LevelWarn {
		result = "degraded"
	}
	safe := slog.NewRecord(record.Time, record.Level, event, record.PC)
	safe.AddAttrs(slog.String("event", event), slog.String("result", result))
	for _, attr := range handler.attrs {
		if filtered, accepted := safeLogAttr(attr); accepted {
			safe.AddAttrs(filtered)
		}
	}
	record.Attrs(func(attr slog.Attr) bool {
		if filtered, accepted := safeLogAttr(attr); accepted {
			safe.AddAttrs(filtered)
		}
		return true
	})
	if requestID := RequestID(ctx); requestID != "" {
		safe.AddAttrs(slog.String("request_id", requestID))
	}
	if traceID := TraceID(ctx); traceID != "" {
		safe.AddAttrs(slog.String("trace_id", traceID))
	}
	return handler.inner.Handle(ctx, safe)
}

func (handler *safeLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *handler
	clone.attrs = append(append([]slog.Attr(nil), handler.attrs...), attrs...)
	return &clone
}

func (handler *safeLogHandler) WithGroup(string) slog.Handler { return handler }

func safeLogAttr(attr slog.Attr) (slog.Attr, bool) {
	attr.Value = attr.Value.Resolve()
	if _, allowed := safeLogFields[attr.Key]; !allowed {
		return slog.Attr{}, false
	}
	switch attr.Value.Kind() {
	case slog.KindInt64, slog.KindUint64, slog.KindFloat64, slog.KindBool, slog.KindDuration, slog.KindTime:
		return attr, true
	case slog.KindString:
		value := strings.TrimSpace(attr.Value.String())
		if !safeLogText.MatchString(value) {
			return slog.Attr{}, false
		}
		return slog.String(attr.Key, value), true
	default:
		return slog.Attr{}, false
	}
}

func boundedLogText(value, fallback string) string {
	value = strings.TrimSpace(value)
	if !safeLogText.MatchString(value) {
		return fallback
	}
	return value
}
