package billing

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
)

type snapshotAdjustmentRepository struct {
	Repository
	adjustment ManualAdjustment
	executions int
}

func (r *snapshotAdjustmentRepository) AdjustmentForExecution(context.Context, uuid.UUID) (ManualAdjustment, error) {
	return r.adjustment, nil
}

func (r *snapshotAdjustmentRepository) ExecuteApprovedAdjustment(_ context.Context, id, approvalID uuid.UUID, key string, approver uuid.UUID, reason string) (ManualAdjustment, error) {
	r.executions++
	if id != r.adjustment.ID || key != "approval:controlled-key" {
		return ManualAdjustment{}, ErrConflict
	}
	if r.adjustment.Status == "POSTED" && (r.adjustment.ApprovalRequestID == nil || *r.adjustment.ApprovalRequestID != approvalID || r.adjustment.ApprovedBy == nil || *r.adjustment.ApprovedBy != approver || r.adjustment.ExecutionIdempotencyKey == nil || *r.adjustment.ExecutionIdempotencyKey != key) {
		return ManualAdjustment{}, ErrConflict
	}
	if r.adjustment.Status != "POSTED" {
		journalID := uuid.New()
		r.adjustment.Status = "POSTED"
		r.adjustment.JournalEntryID = &journalID
		r.adjustment.ApprovalRequestID = &approvalID
		r.adjustment.ApprovedBy = &approver
		r.adjustment.ExecutionIdempotencyKey = &key
		r.adjustment.Version++
	}
	return r.adjustment, nil
}

type snapshotExecutionGate struct {
	command             ApprovedCommand
	job                 string
	begins, completions int
	result              string
}

func (g *snapshotExecutionGate) Begin(context.Context, uuid.UUID, string, json.RawMessage) (ApprovedCommand, error) {
	g.begins++
	return g.command, nil
}

func (g *snapshotExecutionGate) Complete(_ context.Context, _ uuid.UUID, job, result string, _, _ json.RawMessage, _ string) error {
	g.completions++
	g.result = result
	if job != g.job {
		return approvalGateHTTPError{StatusCode: 409, Code: "APPROVAL_JOB_MISMATCH"}
	}
	return nil
}

func snapshotExecutionFixture(t *testing.T) (*Service, *snapshotAdjustmentRepository, *snapshotExecutionGate, uuid.UUID) {
	t.Helper()
	adjustment := ManualAdjustment{ID: uuid.New(), WalletID: uuid.New(), RequestedBy: uuid.New(), AmountMicrocredits: 100, Status: "PENDING_APPROVAL", Version: 7}
	requestID := uuid.New()
	normalized, err := json.Marshal(map[string]any{"adjustment_id": adjustment.ID, "wallet_id": adjustment.WalletID, "requested_by": adjustment.RequestedBy, "amount_microcredits": adjustment.AmountMicrocredits})
	if err != nil {
		t.Fatal(err)
	}
	repo := &snapshotAdjustmentRepository{adjustment: adjustment}
	gate := &snapshotExecutionGate{job: "original-job", command: ApprovedCommand{ApprovalRequestID: requestID, CommandType: "WALLET_ADJUSTMENT", TargetResourceType: "MANUAL_ADJUSTMENT", TargetResourceID: adjustment.ID.String(), NormalizedCommand: normalized, RequesterUserID: adjustment.RequestedBy, ApproverUserID: uuid.New(), DecisionReason: "controlled decision", ExecutionIdempotencyKey: "approval:controlled-key", Status: "EXECUTING"}}
	return NewService(repo).WithApprovalExecutionGate(gate), repo, gate, requestID
}

func TestAdjustmentSnapshotRejectionDoesNotClaimOrComplete(t *testing.T) {
	for _, status := range []string{"PENDING_APPROVAL", "POSTED"} {
		for _, observed := range []string{"null", "{}", `{"adjustment_version":6}`, `{"adjustment_version":"7"}`} {
			t.Run(status+observed, func(t *testing.T) {
				service, repo, gate, requestID := snapshotExecutionFixture(t)
				repo.adjustment.Status = status
				before := repo.adjustment
				_, err := service.executeApprovedAdjustmentCommand(context.Background(), before.ID, requestID, "different-job", json.RawMessage(observed))
				if !errors.Is(err, ErrApprovalSnapshotMismatch) {
					t.Fatalf("rejection: %v", err)
				}
				if gate.begins != 0 || gate.completions != 0 || repo.executions != 0 || repo.adjustment != before {
					t.Fatal("invalid observation mutated execution or domain")
				}
			})
		}
	}
}

func TestAdjustmentValidRetryPreservesPostedReceiptAndOriginalJob(t *testing.T) {
	for _, job := range []string{"original-job", "different-job"} {
		t.Run(job, func(t *testing.T) {
			service, repo, gate, requestID := snapshotExecutionFixture(t)
			key, journalID := "approval:controlled-key", uuid.New()
			repo.adjustment.Status = "POSTED"
			repo.adjustment.JournalEntryID = &journalID
			repo.adjustment.ApprovalRequestID = &requestID
			repo.adjustment.ApprovedBy = &gate.command.ApproverUserID
			repo.adjustment.ExecutionIdempotencyKey = &key
			before := repo.adjustment
			_, err := service.executeApprovedAdjustmentCommand(context.Background(), before.ID, requestID, job, json.RawMessage(`{"adjustment_version":7}`))
			if job == "original-job" && err != nil {
				t.Fatal(err)
			}
			if job != "original-job" && !errors.Is(err, ErrApprovalCompletionPending) {
				t.Fatalf("job fence: %v", err)
			}
			if gate.begins != 1 || gate.completions != 1 || gate.result != "EXECUTED" || repo.adjustment != before {
				t.Fatal("posted receipt lost or falsely failed")
			}
		})
	}
}

func TestAdjustmentCorrectObservationStillExecutes(t *testing.T) {
	service, repo, gate, requestID := snapshotExecutionFixture(t)
	posted, err := service.executeApprovedAdjustmentCommand(context.Background(), repo.adjustment.ID, requestID, "original-job", json.RawMessage(`{"adjustment_version":7}`))
	if err != nil || posted.Status != "POSTED" || posted.JournalEntryID == nil || posted.Version != 8 {
		t.Fatalf("valid command changed: %v", err)
	}
	if gate.begins != 1 || gate.completions != 1 || gate.result != "EXECUTED" || repo.executions != 1 {
		t.Fatal("valid command did not retain the original execution and completion path")
	}
}
