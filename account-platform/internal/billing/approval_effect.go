package billing

import (
	"context"
	"net/http"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvaleffect"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

type ApprovalEffectReader interface {
	ObserveApprovalEffect(context.Context, uuid.UUID, string) (bool, time.Time, error)
}

func (r *PostgresRepository) ObserveApprovalEffect(ctx context.Context, requestID uuid.UUID, commandType string) (bool, time.Time, error) {
	var observed bool
	var observedAt time.Time
	var err error
	switch commandType {
	case "WALLET_ADJUSTMENT":
		err = r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM manual_adjustments WHERE approval_request_id=$1),transaction_timestamp()`, requestID).Scan(&observed, &observedAt)
	case "PAYMENT_PROVIDER_RESUME":
		err = r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM payment_provider_resume_executions WHERE approval_request_id=$1),transaction_timestamp()`, requestID).Scan(&observed, &observedAt)
	case "PAYMENT_PRODUCT_RESUME":
		err = r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM payment_product_resume_executions WHERE approval_request_id=$1),transaction_timestamp()`, requestID).Scan(&observed, &observedAt)
	default:
		err = ErrConflict
	}
	return observed, observedAt.UTC(), err
}

func (s *Service) WithApprovalEffectReader(reader ApprovalEffectReader) *Service {
	s.approvalEffects = reader
	return s
}

func (s *Service) approvalEffectObservation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	workload, ok := serviceauth.FromContext(r.Context())
	if !ok || workload.ActorType != "SERVICE" || (workload.Service != "account-service" && workload.Service != "account-approval-recovery") {
		api.WriteError(w, http.StatusForbidden, "SERVICE_AUTH_FORBIDDEN", "Account or recovery service identity is required", httpserver.RequestID(r.Context()))
		return
	}
	requestID, err := uuid.Parse(r.PathValue("approval_request_id"))
	query := r.URL.Query()
	commandType, fingerprint := query.Get("command_type"), query.Get("command_fingerprint")
	authority, known := approvaleffect.AuthorityForCommand(commandType)
	if err != nil || requestID == uuid.Nil || !known || authority != approvaleffect.AuthorityBilling || !approvaleffect.ValidFingerprint(fingerprint) ||
		len(query) != 2 || len(query["command_type"]) != 1 || len(query["command_fingerprint"]) != 1 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_EFFECT_QUERY", "the approval effect query is invalid", httpserver.RequestID(r.Context()))
		return
	}
	if s.approvalEffects == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_EFFECT_UNAVAILABLE", "the approval effect authority is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	observed, observedAt, err := s.approvalEffects.ObserveApprovalEffect(r.Context(), requestID, commandType)
	if err != nil || observedAt.IsZero() {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_EFFECT_UNAVAILABLE", "the approval effect authority is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	state := approvaleffect.StateAbsent
	if observed {
		state = approvaleffect.StateObserved
	}
	observation := approvaleffect.Observation{SchemaVersion: approvaleffect.SchemaVersion, Authority: approvaleffect.AuthorityBilling,
		ApprovalRequestID: requestID, CommandType: commandType, CommandFingerprint: fingerprint, State: state, ObservedAt: observedAt.UTC()}
	if observation.Validate() != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_EFFECT_UNAVAILABLE", "the approval effect authority is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteJSON(w, http.StatusOK, observation)
}
