package account

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

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
