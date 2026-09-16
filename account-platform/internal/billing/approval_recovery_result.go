package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvaleffect"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

// This is a positive-result authority only. UNCONFIRMED never proves that a
// previously dispatched Provider probe or Wallet transaction had no effect.
type billingRecoveryResult struct {
	SchemaVersion     int             `json:"schema_version"`
	ApprovalRequestID uuid.UUID       `json:"approval_request_id"`
	OriginalEventID   uuid.UUID       `json:"original_event_id"`
	ApprovalEpoch     int64           `json:"approval_epoch"`
	CommandType       string          `json:"command_type"`
	State             string          `json:"state"`
	DomainResult      json.RawMessage `json:"domain_result,omitempty"`
	LedgerReferences  json.RawMessage `json:"ledger_references,omitempty"`
	ObservedAt        time.Time       `json:"observed_at"`
}

type billingRecoveryResultObservation struct {
	State            string
	DomainResult     json.RawMessage
	LedgerReferences json.RawMessage
	ObservedAt       time.Time
}

type billingRecoveryResultReader interface {
	ObserveBillingApprovalResult(context.Context, uuid.UUID, uuid.UUID, int64, string) (billingRecoveryResultObservation, error)
}

func (r *PostgresRepository) ObserveBillingApprovalResult(ctx context.Context, requestID, eventID uuid.UUID, epoch int64, commandType string) (billingRecoveryResultObservation, error) {
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
		return billingRecoveryResultObservation{}, ErrConflict
	}
	err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing_approval_proof_claims WHERE approval_request_id=$1 AND authorization_event_id=$2 AND approval_epoch=$3 AND command_type=$4), EXISTS(`+resultSQL+`), transaction_timestamp()`,
		requestID, eventID, epoch, commandType).Scan(&claimed, &succeeded, &observedAt)
	if err != nil {
		return billingRecoveryResultObservation{}, err
	}
	if claimed && succeeded {
		result, found, err := (&billingAuthorizationDomain{database: r.pool}).LookupAuthorizationResult(ctx,
			ApprovalAuthorizationBinding{ApprovalRequestID: requestID, CommandType: commandType}, approvalencoding.AuthorizationEvent{})
		if err != nil || !found || len(result.DomainResult) == 0 || len(result.LedgerReferences) == 0 {
			return billingRecoveryResultObservation{}, ErrApprovalExecutionUnavailable
		}
		return billingRecoveryResultObservation{State: "SUCCEEDED", DomainResult: result.DomainResult,
			LedgerReferences: result.LedgerReferences, ObservedAt: observedAt.UTC()}, nil
	}
	return billingRecoveryResultObservation{State: "UNCONFIRMED", ObservedAt: observedAt.UTC()}, nil
}

func (s *Service) billingApprovalRecoveryResult(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	workload, ok := serviceauth.FromContext(r.Context())
	if !ok || workload.ActorType != "SERVICE" || (workload.Service != "account-approval-recovery" && workload.Service != "account-service") {
		api.WriteError(w, http.StatusForbidden, "SERVICE_AUTH_FORBIDDEN", "Account or recovery identity is required", httpserver.RequestID(r.Context()))
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
	result, err := reader.ObserveBillingApprovalResult(r.Context(), requestID, eventID, epoch, commandType)
	if err != nil || result.ObservedAt.IsZero() || result.ObservedAt.After(time.Now().Add(time.Second)) ||
		(result.State != "SUCCEEDED" && result.State != "UNCONFIRMED") ||
		(result.State == "SUCCEEDED" && (len(result.DomainResult) == 0 || len(result.LedgerReferences) == 0)) ||
		(result.State == "UNCONFIRMED" && (len(result.DomainResult) != 0 || len(result.LedgerReferences) != 0)) {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_RECOVERY_RESULT_UNAVAILABLE", "recovery result unavailable", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteJSON(w, http.StatusOK, billingRecoveryResult{SchemaVersion: 1, ApprovalRequestID: requestID,
		OriginalEventID: eventID, ApprovalEpoch: epoch, CommandType: commandType, State: result.State,
		DomainResult: result.DomainResult, LedgerReferences: result.LedgerReferences, ObservedAt: result.ObservedAt})
}
