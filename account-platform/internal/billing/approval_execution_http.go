package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

var (
	ErrApprovalExecutionUnavailable = errors.New("approved command execution is unavailable")
	ErrApprovalNotExecutable        = errors.New("approval is not executable")
	ErrApprovalSnapshotMismatch     = errors.New("approved command snapshot mismatch")
	ErrApprovalCompletionPending    = errors.New("approval execution completion is pending")
)

func (s *Service) executeApprovedAdjustment(w http.ResponseWriter, r *http.Request) {
	adjustmentID, pathErr := uuid.Parse(r.PathValue("adjustment_id"))
	var body struct {
		ApprovalRequestID uuid.UUID       `json:"approval_request_id"`
		JobRunID          string          `json:"job_run_id"`
		ObservedVersions  json.RawMessage `json:"observed_versions"`
	}
	if decodeErr := api.DecodeJSON(w, r, &body); pathErr != nil || decodeErr != nil || body.ApprovalRequestID == uuid.Nil || strings.TrimSpace(body.JobRunID) == "" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid approved adjustment execution", httpserver.RequestID(r.Context()))
		return
	}
	posted, err := s.executeApprovedAdjustmentCommand(r.Context(), adjustmentID, body.ApprovalRequestID, strings.TrimSpace(body.JobRunID), body.ObservedVersions)
	if err != nil {
		if errors.Is(err, ErrApprovalExecutionUnavailable) || errors.Is(err, ErrApprovalCompletionPending) {
			api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_EXECUTION_UNAVAILABLE", "approved command execution is temporarily unavailable", httpserver.RequestID(r.Context()))
			return
		}
		if errors.Is(err, ErrApprovalNotExecutable) || errors.Is(err, ErrApprovalSnapshotMismatch) {
			api.WriteError(w, http.StatusConflict, "APPROVAL_NOT_EXECUTABLE", "the approved command cannot be executed", httpserver.RequestID(r.Context()))
			return
		}
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, posted)
}

func (s *Service) executeApprovedAdjustmentCommand(ctx context.Context, adjustmentID, approvalRequestID uuid.UUID, jobRunID string, observedVersions json.RawMessage) (ManualAdjustment, error) {
	repository, repositoryOK := s.repository.(approvalAdjustmentRepository)
	if !repositoryOK || s.approvalGate == nil {
		return ManualAdjustment{}, ErrApprovalExecutionUnavailable
	}
	adjustment, err := repository.AdjustmentForExecution(ctx, adjustmentID)
	if err != nil {
		return ManualAdjustment{}, err
	}
	var observed struct {
		AdjustmentVersion int64 `json:"adjustment_version"`
	}
	if json.Unmarshal(observedVersions, &observed) != nil || observed.AdjustmentVersion != adjustment.Version {
		actualVersions, _ := json.Marshal(map[string]any{"adjustment_version": adjustment.Version})
		if _, err := s.approvalGate.Begin(ctx, approvalRequestID, jobRunID, actualVersions); err != nil && approvalGateUnavailable(err) {
			return ManualAdjustment{}, ErrApprovalExecutionUnavailable
		}
		return ManualAdjustment{}, ErrApprovalSnapshotMismatch
	}
	command, err := s.approvalGate.Begin(ctx, approvalRequestID, jobRunID, observedVersions)
	if err != nil {
		if approvalGateUnavailable(err) {
			return ManualAdjustment{}, ErrApprovalExecutionUnavailable
		}
		return ManualAdjustment{}, ErrApprovalNotExecutable
	}
	fail := func(errorClass string) {
		_ = s.approvalGate.Complete(ctx, approvalRequestID, jobRunID, "FAILED", json.RawMessage(`{}`), json.RawMessage(`{}`), errorClass)
	}
	if validateApprovedAdjustmentCommand(command, adjustment) != nil {
		fail("COMMAND_SNAPSHOT_MISMATCH")
		return ManualAdjustment{}, ErrApprovalSnapshotMismatch
	}
	posted, err := repository.ExecuteApprovedAdjustment(ctx, adjustmentID, approvalRequestID, command.ExecutionIdempotencyKey, command.ApproverUserID, command.DecisionReason)
	if err != nil {
		fail("DOMAIN_EXECUTION_FAILED")
		return ManualAdjustment{}, err
	}
	domainResult, _ := json.Marshal(map[string]any{"adjustment_id": posted.ID, "wallet_id": posted.WalletID, "status": posted.Status, "version": posted.Version})
	ledgerReferences, _ := json.Marshal(map[string]any{"journal_entry_id": posted.JournalEntryID})
	if err := s.approvalGate.Complete(ctx, approvalRequestID, jobRunID, "EXECUTED", domainResult, ledgerReferences, ""); err != nil {
		return ManualAdjustment{}, ErrApprovalCompletionPending
	}
	return posted, nil
}

func validateApprovedAdjustmentCommand(command ApprovedCommand, adjustment ManualAdjustment) error {
	var normalized struct {
		AdjustmentID       uuid.UUID `json:"adjustment_id"`
		WalletID           uuid.UUID `json:"wallet_id"`
		AmountMicrocredits int64     `json:"amount_microcredits"`
		RequestedBy        uuid.UUID `json:"requested_by"`
	}
	if command.CommandType != "WALLET_ADJUSTMENT" || command.TargetResourceType != "MANUAL_ADJUSTMENT" || command.TargetResourceID != adjustment.ID.String() || json.Unmarshal(command.NormalizedCommand, &normalized) != nil || normalized.AdjustmentID != adjustment.ID || normalized.WalletID != adjustment.WalletID || normalized.AmountMicrocredits != adjustment.AmountMicrocredits || normalized.RequestedBy != adjustment.RequestedBy || command.RequesterUserID != adjustment.RequestedBy || command.ApproverUserID == uuid.Nil || strings.TrimSpace(command.DecisionReason) == "" || strings.TrimSpace(command.ExecutionIdempotencyKey) == "" {
		return errors.New("approved command snapshot does not match adjustment")
	}
	return nil
}
