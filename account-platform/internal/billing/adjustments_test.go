package billing

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestValidateAdjustmentDecisionRequiresSecondActorAndReason(t *testing.T) {
	requester := uuid.New()
	adjustment := ManualAdjustment{RequestedBy: requester, Status: "PENDING_APPROVAL"}

	if err := validateAdjustmentDecision(adjustment, requester, "approved"); !errors.Is(err, ErrConflict) {
		t.Fatalf("requester must not approve their own adjustment: %v", err)
	}
	if err := validateAdjustmentDecision(adjustment, uuid.New(), "  "); !errors.Is(err, ErrConflict) {
		t.Fatalf("approval reason must be required: %v", err)
	}
	if err := validateAdjustmentDecision(adjustment, uuid.New(), "independent review complete"); err != nil {
		t.Fatalf("independent approval should be accepted: %v", err)
	}
}

func TestValidateAdjustmentDecisionRejectsTerminalState(t *testing.T) {
	adjustment := ManualAdjustment{RequestedBy: uuid.New(), Status: "POSTED"}
	if err := validateAdjustmentDecision(adjustment, uuid.New(), "reviewed"); !errors.Is(err, ErrConflict) {
		t.Fatalf("terminal adjustment must not be decided again: %v", err)
	}
}
