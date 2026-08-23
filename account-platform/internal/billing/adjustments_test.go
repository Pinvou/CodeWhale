package billing

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
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

func TestApprovalConsumerFailsClosedForWrongExecutor(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{"approval_request_id": uuid.New(), "command_type": "WALLET_ADJUSTMENT", "target_resource_type": "MANUAL_ADJUSTMENT", "target_resource_id": uuid.New(), "critical_resource_versions": map[string]any{"adjustment_version": 1}, "executor_identity": "unknown-executor"})
	event := events.Event{ID: uuid.New(), Type: "account.approval-command.authorized.v1", AggregateID: uuid.New(), AggregateVersion: 2, TraceID: "test", Payload: payload}
	if err := NewApprovalCommandConsumer(&Service{}).Handle(context.Background(), event); err == nil {
		t.Fatal("wrong executor identity must fail closed")
	}
}

func TestApprovedAdjustmentCommandMustMatchFrozenFinancialFacts(t *testing.T) {
	adjustment := ManualAdjustment{ID: uuid.New(), WalletID: uuid.New(), AmountMicrocredits: 2400, Version: 1}
	approver := uuid.New()
	adjustment.RequestedBy = uuid.New()
	raw, _ := json.Marshal(map[string]any{"adjustment_id": adjustment.ID, "wallet_id": adjustment.WalletID, "amount_microcredits": adjustment.AmountMicrocredits, "requested_by": adjustment.RequestedBy})
	command := ApprovedCommand{CommandType: "WALLET_ADJUSTMENT", TargetResourceType: "MANUAL_ADJUSTMENT", TargetResourceID: adjustment.ID.String(), NormalizedCommand: raw, RequesterUserID: adjustment.RequestedBy, ApproverUserID: approver, DecisionReason: "independent review complete", ExecutionIdempotencyKey: "approval:stable"}
	if err := validateApprovedAdjustmentCommand(command, adjustment); err != nil {
		t.Fatalf("matching frozen command should execute: %v", err)
	}
	command.NormalizedCommand = json.RawMessage(`{"adjustment_id":"` + adjustment.ID.String() + `","wallet_id":"` + adjustment.WalletID.String() + `","amount_microcredits":2401,"requested_by":"` + adjustment.RequestedBy.String() + `"}`)
	if err := validateApprovedAdjustmentCommand(command, adjustment); err == nil {
		t.Fatal("amount drift must fail closed")
	}
	command.NormalizedCommand = raw
	command.TargetResourceID = uuid.NewString()
	if err := validateApprovedAdjustmentCommand(command, adjustment); err == nil {
		t.Fatal("target substitution must fail closed")
	}
}

func TestValidateAdjustmentDecisionRejectsTerminalState(t *testing.T) {
	adjustment := ManualAdjustment{RequestedBy: uuid.New(), Status: "POSTED"}
	if err := validateAdjustmentDecision(adjustment, uuid.New(), "reviewed"); !errors.Is(err, ErrConflict) {
		t.Fatalf("terminal adjustment must not be decided again: %v", err)
	}
}
