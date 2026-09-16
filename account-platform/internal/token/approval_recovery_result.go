package token

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvaleffect"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

// A missing row is deliberately UNCONFIRMED, never proof of zero effect. The
// original idempotency key is sent only as a digest, not in a URL or log.
type tokenRecoveryResult struct {
	SchemaVersion      int       `json:"schema_version"`
	ApprovalRequestID  uuid.UUID `json:"approval_request_id"`
	CommandType        string    `json:"command_type"`
	CommandFingerprint string    `json:"command_fingerprint"`
	OriginalKeyDigest  string    `json:"original_key_digest"`
	State              string    `json:"state"`
	ObservedAt         time.Time `json:"observed_at"`
}

type tokenRecoveryResultReader interface {
	ObserveTokenApprovalResult(context.Context, uuid.UUID, string, string, string) (string, time.Time, error)
}

func (r *PostgresRepository) ObserveTokenApprovalResult(ctx context.Context, requestID uuid.UUID, commandType, fingerprint, keyDigest string) (string, time.Time, error) {
	var state string
	var observedAt time.Time
	err := r.pool.QueryRow(ctx, `SELECT COALESCE((SELECT status FROM token_command_results
WHERE approval_request_id=$1 AND command_type=$2 AND approval_command_fingerprint=$3 AND idempotency_key_digest=$4), 'UNCONFIRMED'), transaction_timestamp()`,
		requestID, commandType, fingerprint, keyDigest).Scan(&state, &observedAt)
	return state, observedAt.UTC(), err
}

func (s *Service) tokenApprovalRecoveryResult(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	workload, ok := serviceauth.FromContext(r.Context())
	if !ok || workload.ActorType != "SERVICE" || workload.Service != "account-approval-recovery" {
		api.WriteError(w, http.StatusForbidden, "SERVICE_AUTH_FORBIDDEN", "recovery identity is required", httpserver.RequestID(r.Context()))
		return
	}
	requestID, err := uuid.Parse(r.PathValue("approval_request_id"))
	query := r.URL.Query()
	commandType, fingerprint, keyDigest := query.Get("command_type"), query.Get("command_fingerprint"), query.Get("original_key_digest")
	authority, known := approvaleffect.AuthorityForCommand(commandType)
	if err != nil || requestID == uuid.Nil || !known || authority != approvaleffect.AuthorityToken ||
		!approvaleffect.ValidFingerprint(fingerprint) || !approvaleffect.ValidFingerprint(keyDigest) ||
		len(query) != 3 || len(query["command_type"]) != 1 || len(query["command_fingerprint"]) != 1 || len(query["original_key_digest"]) != 1 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_RECOVERY_RESULT", "invalid recovery result query", httpserver.RequestID(r.Context()))
		return
	}
	reader, ok := s.repository.(tokenRecoveryResultReader)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_RECOVERY_RESULT_UNAVAILABLE", "recovery result unavailable", httpserver.RequestID(r.Context()))
		return
	}
	state, observedAt, err := reader.ObserveTokenApprovalResult(r.Context(), requestID, commandType, fingerprint, keyDigest)
	if err != nil || observedAt.IsZero() || observedAt.After(time.Now().Add(time.Second)) ||
		!strings.Contains("|UNCONFIRMED|PENDING|SUCCEEDED|REJECTED|FAILED|UNKNOWN|", "|"+state+"|") {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_RECOVERY_RESULT_UNAVAILABLE", "recovery result unavailable", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteJSON(w, http.StatusOK, tokenRecoveryResult{SchemaVersion: 1, ApprovalRequestID: requestID,
		CommandType: commandType, CommandFingerprint: fingerprint, OriginalKeyDigest: keyDigest, State: state, ObservedAt: observedAt})
}
