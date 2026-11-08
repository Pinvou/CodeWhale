package token

import "testing"

func TestTrustedUsageDeadlineReason(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
		want   bool
	}{
		{"released-evidence-unavailable", "TRUSTED_USAGE_DEADLINE_EXCEEDED:USAGE_EVIDENCE_UNAVAILABLE", true},
		{"released-evidence-timeout", "TRUSTED_USAGE_DEADLINE_EXCEEDED:USAGE_EVIDENCE_TIMEOUT", true},
		{"release-unavailable", "HOLD_RELEASE_UNAVAILABLE:USAGE_EVIDENCE_UNAVAILABLE", false},
		{"nested-deadline-text", "HOLD_RELEASE_UNAVAILABLE:TRUSTED_USAGE_DEADLINE_EXCEEDED:USAGE_EVIDENCE_UNAVAILABLE", false},
		{"manual-review", "MANUAL_RECONCILIATION_REQUIRED", false},
		{"release-human-reason", "trusted usage deadline exceeded", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := trustedUsageDeadlineExceeded(tc.reason); got != tc.want {
				t.Fatalf("deadline reason classification = %t, want %t", got, tc.want)
			}
		})
	}
}
