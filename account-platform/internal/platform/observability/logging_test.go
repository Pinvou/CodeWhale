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

func TestRecoverySummaryRetainsCountsWithoutRecoverySecrets(t *testing.T) {
	var output bytes.Buffer
	NewLogger(&output, Config{Service: "account-approval-recovery", Environment: "test"}).Info("approval recovery pass",
		"candidates", 2, "confirmed", 1, "pending", 1, "unavailable", 0, "invalidated", 0, "more_candidates", true,
		"lease_token", "secret-recovery-marker", "idempotency_key", "secret-recovery-marker", "evidence", "secret-recovery-marker")
	var entry map[string]any
	if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["event"] != "approval_recovery_pass" || entry["candidates"] != float64(2) || entry["confirmed"] != float64(1) || entry["pending"] != float64(1) || entry["more_candidates"] != true || strings.Contains(output.String(), "secret-recovery-marker") {
		t.Fatalf("unsafe or unusable recovery summary: %s", output.String())
	}
}

func TestLoggerKeepsRedactedRegistrationConflictEvidence(t *testing.T) {
	var output bytes.Buffer
	logger := NewLogger(&output, Config{Service: "account-service", Environment: "test", Version: "dev", Commit: "test"})
	logger.Warn("registration operation conflict",
		"stage", "fingerprint_check",
		"operation_id", "01a03da2-2ef6-780b-9de2-f388e0109b38",
		"stored_fingerprint", "21d478669dde6e5bdffd51115f2a1c9588a71f68ff20c34fc841b81548c0505a",
		"calculated_fingerprint", "21d478669dde6e5bdffd51115f2a1c9588a71f68ff20c34fc841b81548c0505a",
		"mobile", "+8619919890802",
	)
	logged := output.String()
	for _, expected := range []string{"registration_operation_conflict", "fingerprint_check", "01a03da2-2ef6-780b-9de2-f388e0109b38", "stored_fingerprint", "calculated_fingerprint"} {
		if !strings.Contains(logged, expected) {
			t.Fatalf("missing %q from registration conflict log: %s", expected, logged)
		}
	}
	if strings.Contains(logged, "+8619919890802") || strings.Contains(logged, `"mobile"`) {
		t.Fatalf("sensitive mobile field leaked: %s", logged)
	}
}
