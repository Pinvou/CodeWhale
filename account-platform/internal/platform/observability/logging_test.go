package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestLoggerUsesStableSchemaAndDropsUnapprovedFields(t *testing.T) {
	const marker = "secret-marker-should-never-appear"
	var output bytes.Buffer
	logger := NewLogger(&output, Config{Service: "test-service", Environment: "test", Version: "v1", Commit: "abc123"})
	ctx := WithCorrelation(context.Background(), "request-123", "4bf92f3577b34da6a3ce929d0e0e4736")
	logger.ErrorContext(ctx, "mobile authentication request failed",
		"error", errors.New(marker),
		"password", marker,
		"request_body", marker,
		"error_code", "AUTHENTICATION_FAILED",
	)
	if strings.Contains(output.String(), marker) {
		t.Fatalf("protected marker leaked into log: %s", output.String())
	}
	var entry map[string]any
	if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"time", "level", "service", "environment", "version", "event", "result", "request_id", "trace_id"} {
		if entry[key] == nil || entry[key] == "" {
			t.Fatalf("required field %q missing from %#v", key, entry)
		}
	}
	if entry["event"] != "mobile_authentication_request_failed" || entry["result"] != "failure" {
		t.Fatalf("unexpected stable event: %#v", entry)
	}
}

func TestLoggerCollapsesUnknownDynamicMessages(t *testing.T) {
	const marker = "dynamic-secret-event-marker"
	var output bytes.Buffer
	NewLogger(&output, Config{Service: "test", Environment: "test", Version: "v1", Commit: "abc"}).Log(context.Background(), slog.LevelInfo, marker)
	if strings.Contains(output.String(), marker) || !strings.Contains(output.String(), "application_event") {
		t.Fatalf("dynamic message was not safely collapsed: %s", output.String())
	}
}
