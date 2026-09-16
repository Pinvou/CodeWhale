package billing

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvaleffect"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

// This is a positive-result authority only. UNCONFIRMED never proves that a
// previously dispatched Provider probe or Wallet transaction had no effect.
type billingRecoveryResult struct {
	SchemaVersion     int       `json:"schema_version"`
	ApprovalRequestID uuid.UUID `json:"approval_request_id"`
	OriginalEventID   uuid.UUID `json:"original_event_id"`
	ApprovalEpoch     int64     `json:"approval_epoch"`
	CommandType       string    `json:"command_type"`
	State             string    `json:"state"`
	ObservedAt        time.Time `json:"observed_at"`
}

type billingRecoveryResultReader interface {
	ObserveBillingApprovalResult(context.Context, uuid.UUID, uuid.UUID, int64, string) (string, time.Time, error)
}

func (r *PostgresRepository) ObserveBillingApprovalResult(ctx context.Context, requestID, eventID uuid.UUID, epoch int64, commandType string) (string, time.Time, error) {
	var claimed, succeeded bool
	var observedAt time.Time
	var resultSQL string
	switch commandType {
	case "WALLET_ADJUSTMENT":
		resultSQL = `SELECT 1 FROM manual_adjustments WHERE approval_request_id=$1 AND status='POSTED' AND journal_entry_id IS NOT NULL AND execution_idempotency_key IS NOT NULL`
	case "PAYMENT_PROVIDER_RESUME":
		resultSQL = `SELECT 1 FROM payment_provider_resume_executions WHERE approval_request_id=$1`
	case "PAYMENT_PRODUCT_RESUME":
		resultSQL = `SELECT 1 FROM payment_product_resume_executions WHERE approval_request_id=$1`
	default:
		return "", time.Time{}, ErrConflict
	}
	err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing_approval_proof_claims WHERE approval_request_id=$1 AND authorization_event_id=$2 AND approval_epoch=$3 AND command_type=$4), EXISTS(`+resultSQL+`), transaction_timestamp()`,
		requestID, eventID, epoch, commandType).Scan(&claimed, &succeeded, &observedAt)
	if err != nil {
		return "", time.Time{}, err
	}
	if claimed && succeeded {
		return "SUCCEEDED", observedAt.UTC(), nil
	}
	return "UNCONFIRMED", observedAt.UTC(), nil
}

func (s *Service) billingApprovalRecoveryResult(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	workload, ok := serviceauth.FromContext(r.Context())
	if !ok || workload.ActorType != "SERVICE" || workload.Service != "account-approval-recovery" {
		api.WriteError(w, http.StatusForbidden, "SERVICE_AUTH_FORBIDDEN", "recovery identity is required", httpserver.RequestID(r.Context()))
		return
	}
	requestID, requestErr := uuid.Parse(r.PathValue("approval_request_id"))
	query := r.URL.Query()
	eventID, eventErr := uuid.Parse(query.Get("original_event_id"))
	epoch, epochErr := strconv.ParseInt(query.Get("approval_epoch"), 10, 64)
	commandType := query.Get("command_type")
	authority, known := approvaleffect.AuthorityForCommand(commandType)
	if requestErr != nil || eventErr != nil || epochErr != nil || requestID == uuid.Nil || eventID == uuid.Nil || epoch < 1 ||
		strconv.FormatInt(epoch, 10) != query.Get("approval_epoch") || !known || authority != approvaleffect.AuthorityBilling ||
		len(query) != 3 || len(query["original_event_id"]) != 1 || len(query["approval_epoch"]) != 1 || len(query["command_type"]) != 1 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_RECOVERY_RESULT", "invalid recovery result query", httpserver.RequestID(r.Context()))
		return
	}
	reader, ok := s.repository.(billingRecoveryResultReader)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_RECOVERY_RESULT_UNAVAILABLE", "recovery result unavailable", httpserver.RequestID(r.Context()))
		return
	}
	state, observedAt, err := reader.ObserveBillingApprovalResult(r.Context(), requestID, eventID, epoch, commandType)
	if err != nil || observedAt.IsZero() || observedAt.After(time.Now().Add(time.Second)) || (state != "SUCCEEDED" && state != "UNCONFIRMED") {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_RECOVERY_RESULT_UNAVAILABLE", "recovery result unavailable", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteJSON(w, http.StatusOK, billingRecoveryResult{SchemaVersion: 1, ApprovalRequestID: requestID,
		OriginalEventID: eventID, ApprovalEpoch: epoch, CommandType: commandType, State: state, ObservedAt: observedAt})
}
