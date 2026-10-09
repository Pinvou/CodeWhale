package account

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

func TestAccountClosureRecentAuthenticationHTTPErrorIsStable(t *testing.T) {
	recorder := httptest.NewRecorder()
	(&Service{}).writeError(recorder, httptest.NewRequest(http.MethodPost, "/v1/account-closure", nil), ErrRecentAuthenticationRequired)
	var payload struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode error response: %v body=%s", err, recorder.Body.String())
	}
	if recorder.Code != http.StatusUnauthorized || payload.Code != "RECENT_AUTHENTICATION_REQUIRED" {
		t.Fatalf("unexpected response: status=%d code=%q body=%s", recorder.Code, payload.Code, recorder.Body.String())
	}
}

func TestAccountClosureValidationClassifiesRecentAuthentication(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	valid := identity.Principal{AuthenticatedAt: now.Add(-time.Minute)}
	tests := []struct {
		name         string
		principal    identity.Principal
		key          string
		confirmation string
		want         error
	}{
		{name: "valid", principal: valid, key: "account-closure-key", confirmation: accountClosureConfirmation},
		{name: "stale authentication", principal: identity.Principal{AuthenticatedAt: now.Add(-accountClosureRecentAuth - time.Second)}, key: "account-closure-key", confirmation: accountClosureConfirmation, want: ErrRecentAuthenticationRequired},
		{name: "missing authentication time", principal: identity.Principal{}, key: "account-closure-key", confirmation: accountClosureConfirmation, want: ErrRecentAuthenticationRequired},
		{name: "future authentication time", principal: identity.Principal{AuthenticatedAt: now.Add(time.Minute + time.Second)}, key: "account-closure-key", confirmation: accountClosureConfirmation, want: ErrRecentAuthenticationRequired},
		{name: "invalid confirmation", principal: valid, key: "account-closure-key", confirmation: "CLOSE_MY_ORGANIZATION", want: ErrForbidden},
		{name: "invalid idempotency key", principal: valid, key: "short", confirmation: accountClosureConfirmation, want: ErrForbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateAccountClosureRequest(now, test.principal, test.key, test.confirmation)
			if (test.want == nil && err != nil) || (test.want != nil && !errors.Is(err, test.want)) {
				t.Fatalf("validation error=%v, want %v", err, test.want)
			}
		})
	}
}

func TestAccountClosureRoleBlockersAreActionable(t *testing.T) {
	tests := []struct {
		err    error
		code   string
		action string
	}{
		{ErrLastOwner, "ORGANIZATION_OWNERSHIP_REQUIRED", "TRANSFER_OWNERSHIP"},
		{ErrLastActiveSuperAdmin, "LAST_ACTIVE_SUPER_ADMIN", "HANDOFF_SUPER_ADMIN"},
	}
	for _, test := range tests {
		blockers, err := accountClosureRoleBlockers(test.err)
		if err != nil || len(blockers) != 1 || blockers[0].Code != test.code || blockers[0].UserAction != test.action {
			t.Fatalf("unexpected blocker for %v: %#v, %v", test.err, blockers, err)
		}
	}
	if _, err := accountClosureRoleBlockers(errors.New("database unavailable")); err == nil {
		t.Fatal("unexpected infrastructure error must not be converted to a business blocker")
	}
}

func TestUserVisibleClosureRemovesInternalIdentifiers(t *testing.T) {
	now := time.Now().UTC()
	item := AccountClosureRequest{
		ID: uuid.New(), UserID: uuid.New(), Status: "BLOCKED", Stage: "FINANCIAL_CLEARANCE",
		PolicyVersion: 1, Attempts: 1, ReauthenticatedAt: now, SLAExpiresAt: now.Add(time.Hour),
		Version: 4, CreatedAt: now, UpdatedAt: now,
		Blockers: []AccountClosureBlocker{{Code: "PAYMENT_PROCESSING", Domain: "BILLING", ResourceType: "PAYMENT_ORDER", ResourceID: uuid.NewString(), UserAction: "FINISH_PAYMENT_ORDER"}},
	}
	visible := userVisibleClosure(item)
	if visible.ID != item.ID || visible.Blockers[0].ResourceID != "" {
		t.Fatalf("unexpected public closure: %#v", visible)
	}
}
