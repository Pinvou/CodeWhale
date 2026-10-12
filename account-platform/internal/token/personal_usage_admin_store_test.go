package token

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestF05AdminUsageDetailRestoredProjectionRemainsRedacted(t *testing.T) {
	diagnostic, session, fingerprint, provider, upstream := "private-provider-failure", "private-session", "private-fingerprint", "private-provider", "private-upstream"
	item := AdminUsageRequest{SessionID: session, RequestFingerprint: fingerprint, ProviderID: &provider, UpstreamRequestID: &upstream, ReconciliationLastError: &diagnostic, CancellationEvidence: json.RawMessage(`{"private_evidence":true}`), ConvergenceEvidence: json.RawMessage(`{"private_evidence":true}`), ExecutionStatus: "REVIEW", ReconciliationAttempts: 3}
	raw, err := json.Marshal(redactAdminUsageDetail(item))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{diagnostic, session, fingerprint, provider, upstream, "private_evidence", "session_id", "credential_id", "request_fingerprint", "wallet_id"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("unsafe usage detail contains %q", secret)
		}
	}
	if !strings.Contains(string(raw), "RECONCILIATION_DIAGNOSTIC_REDACTED") || !strings.Contains(string(raw), `"execution_status":"REVIEW"`) || !strings.Contains(string(raw), `"reconciliation_attempts":3`) {
		t.Fatal("safe operational facts lost")
	}
}
