package token

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAdminUsageDetailRedactsInternalCorrelation(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	item := AdminUsageRequest{
		ID: uuid.MustParse("0199a200-0001-7000-8000-000000000001"), UserID: uuid.MustParse("0199a200-0001-7000-8000-000000000002"),
		SessionID: "private-session", TokenAccountID: uuid.MustParse("0199a200-0001-7000-8000-000000000003"),
		CredentialID: uuid.MustParse("0199a200-0001-7000-8000-000000000004"), WalletID: uuid.MustParse("0199a200-0001-7000-8000-000000000005"),
		Model: "model-one", Status: "REVIEW", RequestFingerprint: "private-request-digest",
		ExecutionStatus: "REVIEW", DeliveryStatus: "COMPLETED", CancelStatus: "NOT_REQUESTED", SettlementStatus: "REVIEW",
		MaximumMicrocredits: 100, ReconciliationAttempts: 2, ProviderID: pointer("private-provider"),
		UpstreamRequestID: pointer("private-upstream-request"), CancellationEvidence: json.RawMessage(`{"secret":"not-inline"}`),
		ConvergenceEvidence: json.RawMessage(`{"secret":"not-inline"}`), ReconciliationLastError: pointer("private customer reason"), StartedAt: now, UpdatedAt: now,
	}
	payload, err := json.Marshal(redactAdminUsageDetail(item))
	if err != nil {
		t.Fatal(err)
	}
	text := string(payload)
	for _, hidden := range []string{"user_id", "session_id", "credential_id", "wallet_id", "request_fingerprint", "provider_id", "upstream_request_id", "cancellation_evidence", "convergence_evidence", "private"} {
		if strings.Contains(text, hidden) {
			t.Fatalf("redacted detail leaked %q: %s", hidden, text)
		}
	}
	for _, required := range []string{"usage_request_id", "token_account_id", "model", "execution_status", "settlement_status", "maximum_microcredits"} {
		if !strings.Contains(text, `"`+required+`"`) {
			t.Fatalf("redacted detail omitted %q: %s", required, text)
		}
	}
	if !strings.Contains(text, "RECONCILIATION_DIAGNOSTIC_REDACTED") {
		t.Fatalf("free-form diagnostic was not replaced by a stable code: %s", text)
	}
}

func TestStableReconciliationDiagnosticUsesControlledVocabulary(t *testing.T) {
	for _, item := range []struct {
		input string
		want  string
	}{
		{input: "MANUAL_RECONCILIATION_REQUESTED", want: "MANUAL_RECONCILIATION_REQUESTED"},
		{input: "USAGE_EVIDENCE_TIMEOUT", want: "USAGE_EVIDENCE_TIMEOUT"},
		{input: "TRUSTED_USAGE_DEADLINE_EXCEEDED:USAGE_EVIDENCE_UNAVAILABLE", want: "TRUSTED_USAGE_DEADLINE_EXCEEDED"},
		{input: "SECRET", want: "RECONCILIATION_DIAGNOSTIC_REDACTED"},
		{input: "API_KEY", want: "RECONCILIATION_DIAGNOSTIC_REDACTED"},
		{input: "PASSWORD", want: "RECONCILIATION_DIAGNOSTIC_REDACTED"},
		{input: "CUSTOMER_PHONE_13800138000", want: "RECONCILIATION_DIAGNOSTIC_REDACTED"},
		{input: "_CODE", want: "RECONCILIATION_DIAGNOSTIC_REDACTED"},
		{input: "1CODE", want: "RECONCILIATION_DIAGNOSTIC_REDACTED"},
	} {
		t.Run(item.input, func(t *testing.T) {
			got := stableReconciliationDiagnostic(&item.input)
			if got == nil || *got != item.want {
				t.Fatalf("diagnostic=%v want=%q", got, item.want)
			}
		})
	}
}

func TestAdminProvisioningDetailExcludesAttemptsAndSensitiveOperations(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	item := RouteProvisioning{
		ID: uuid.MustParse("0199a200-0001-7000-8000-000000000010"), ScopeType: "PERSONAL",
		ScopeID: uuid.MustParse("0199a200-0001-7000-8000-000000000011"), ModelID: uuid.MustParse("0199a200-0001-7000-8000-000000000012"),
		ProtocolCode: "CHAT", DesiredGeneration: 2, Status: "FAILED", Stage: "VERIFY", PolicyVersion: 1,
		AttemptCount: 1, RepairRound: 0, MaxAttempts: 3, SLADeadlineAt: now.Add(time.Hour),
		LeaseUntil: pointer(now.Add(time.Minute)), LastErrorCode: pointer("ROUTE_UNAVAILABLE"), LastErrorDetail: pointer("private provider response"),
		ActiveCredentialID: pointer(uuid.MustParse("0199a200-0001-7000-8000-000000000013")), UpdatedAt: now,
	}
	payload, err := json.Marshal(redactAdminProvisioningDetail(item))
	if err != nil {
		t.Fatal(err)
	}
	text := string(payload)
	for _, hidden := range []string{`"attempts"`, `"worker_id"`, `"error_detail"`, `"last_error_detail"`, `"lease_until"`, `"active_credential_id"`, "private"} {
		if strings.Contains(text, hidden) {
			t.Fatalf("redacted provisioning detail leaked %q: %s", hidden, text)
		}
	}
	for _, required := range []string{"provisioning_id", "model_id", "protocol_code", "desired_generation", "last_error_code"} {
		if !strings.Contains(text, `"`+required+`"`) {
			t.Fatalf("redacted provisioning detail omitted %q: %s", required, text)
		}
	}
}

func TestAdminOperationReasonCountsUnicodeCharacters(t *testing.T) {
	for _, item := range []struct {
		name  string
		value string
		valid bool
	}{
		{name: "one", value: "理", valid: true},
		{name: "five hundred astral", value: strings.Repeat("😀", 500), valid: true},
		{name: "blank", value: " \t\n", valid: false},
		{name: "five hundred one", value: strings.Repeat("理", 501), valid: false},
	} {
		t.Run(item.name, func(t *testing.T) {
			_, valid := normalizeAdminOperationReason(item.value)
			if valid != item.valid {
				t.Fatalf("valid=%t want=%t", valid, item.valid)
			}
		})
	}
}

func pointer[T any](value T) *T { return &value }
