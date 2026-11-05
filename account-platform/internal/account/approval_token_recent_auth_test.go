package account

import (
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
)

func TestTokenApprovalRecentAuthenticationClosedCommands(t *testing.T) {
	token := map[string]bool{"TOKEN_MODEL_LIFECYCLE_CHANGE": true, "TOKEN_MODEL_CATALOG_ROLLBACK": true, "TOKEN_CREDENTIAL_REVOKE": true}
	proof := map[string]bool{"PAYMENT_PROVIDER_RESUME": true, "PAYMENT_PRODUCT_RESUME": true, "ENTITLEMENT_COMPONENT_ADJUSTMENT": true}
	for _, p := range approvalpolicy.All() {
		if got := approvalTokenNeedsRecentAuthentication(p.CommandType); got != token[p.CommandType] {
			t.Errorf("Token recent-auth scope %s = %v", p.CommandType, got)
		}
		if got := approvalProofNeedsRecentAuthentication(p.CommandType); got != (token[p.CommandType] || proof[p.CommandType]) {
			t.Errorf("proof recent-auth scope %s = %v", p.CommandType, got)
		}
	}
	for _, command := range []string{"", "TOKEN_UNKNOWN", "TOKEN_ACCOUNT_CREATE", "TOKEN_MODEL_PUBLISH", "WALLET_ADJUSTMENT", " TOKEN_CREDENTIAL_REVOKE", "token_credential_revoke"} {
		if approvalTokenNeedsRecentAuthentication(command) {
			t.Errorf("unregistered canonical command included: %q", command)
		}
		if err := requireTokenApprovalRecentAuthentication(identity.Principal{}, command, time.Now()); err != nil {
			t.Errorf("unrelated command gained a new age gate: %q: %v", command, err)
		}
	}
}

func TestTokenApprovalRecentAuthenticationBoundary(t *testing.T) {
	now := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	for _, command := range []string{"TOKEN_MODEL_LIFECYCLE_CHANGE", "TOKEN_MODEL_CATALOG_ROLLBACK", "TOKEN_CREDENTIAL_REVOKE"} {
		for _, sample := range []struct {
			name string
			auth time.Time
			deny bool
		}{
			{"missing", time.Time{}, true},
			{"now", now, false},
			{"ten-minutes-is-not-token-precheck", now.Add(-10 * time.Minute), false},
			{"exact-fifteen-minutes", now.Add(-15 * time.Minute), false},
			{"past-fifteen-minutes", now.Add(-15*time.Minute - time.Nanosecond), true},
			{"future-skew-boundary", now.Add(time.Minute), false},
			{"future-skew-exceeded", now.Add(time.Minute + time.Nanosecond), true},
		} {
			t.Run(command+"/"+sample.name, func(t *testing.T) {
				err := requireTokenApprovalRecentAuthentication(identity.Principal{AuthenticatedAt: sample.auth}, command, now)
				if sample.deny && !errors.Is(err, ErrRecentAuthenticationRequired) || !sample.deny && err != nil {
					t.Fatalf("recent-auth boundary: %v", err)
				}
			})
		}
	}
}
