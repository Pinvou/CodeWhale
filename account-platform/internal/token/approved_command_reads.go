package token

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func tokenExecutorRequest(r *http.Request) bool {
	workload, ok := serviceauth.FromContext(r.Context())
	return ok && workload.ActorType == "SERVICE" && workload.Service == "token-command-executor" && strings.TrimSpace(workload.Subject) != ""
}

// Each route reuses its own existing execution permission. These read-only
// projections do not grant a new cross-command enumeration capability.
func (s *Service) approvedTokenCommandPrecheck(commandType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !tokenExecutorRequest(r) {
			api.WriteError(w, 403, "SERVICE_AUTH_FORBIDDEN", "Token executor identity is required", httpserver.RequestID(r.Context()))
			return
		}
		var body ApprovalSnapshotRequest
		if api.DecodeJSON(w, r, &body) != nil || body.CommandType != commandType {
			api.WriteError(w, 400, "INVALID_APPROVED_COMMAND", "the approved command is invalid", httpserver.RequestID(r.Context()))
			return
		}
		projection, err := s.approvalCommandPrecheck(r.Context(), body)
		if err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, ErrCatalogVersionConflict) || errors.Is(err, ErrNotFound) {
				status = http.StatusConflict
			}
			api.WriteError(w, status, "APPROVAL_SNAPSHOT_UNAVAILABLE", "authoritative approval facts are unavailable", httpserver.RequestID(r.Context()))
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		api.WriteJSON(w, http.StatusOK, projection)
	}
}

func (s *Service) approvedTokenCommandResult(commandType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !tokenExecutorRequest(r) {
			api.WriteError(w, 403, "SERVICE_AUTH_FORBIDDEN", "Token executor identity is required", httpserver.RequestID(r.Context()))
			return
		}
		query := r.URL.Query()
		requestID, err := uuid.Parse(query.Get("approval_request_id"))
		fingerprint := query.Get("command_fingerprint")
		if err != nil || requestID == uuid.Nil || !validImpactFingerprint(fingerprint) || len(query) != 2 || len(query["approval_request_id"]) != 1 || len(query["command_fingerprint"]) != 1 {
			api.WriteError(w, 400, "INVALID_APPROVED_COMMAND", "the approved command reference is invalid", httpserver.RequestID(r.Context()))
			return
		}
		repository, ok := s.repository.(*PostgresRepository)
		if !ok {
			api.WriteError(w, 503, "COMMAND_RESULT_UNAVAILABLE", "the original result is unavailable", httpserver.RequestID(r.Context()))
			return
		}
		item, err := scanTokenCommandResult(repository.pool.QueryRow(r.Context(), tokenCommandResultSelect+` WHERE approval_request_id=$1 AND command_type=$2 AND approval_command_fingerprint=$3`, requestID, commandType, fingerprint))
		if errors.Is(err, pgx.ErrNoRows) {
			api.WriteError(w, 404, "COMMAND_RESULT_UNCONFIRMED", "the original result is not confirmed", httpserver.RequestID(r.Context()))
			return
		}
		if err != nil {
			api.WriteError(w, 503, "COMMAND_RESULT_UNAVAILABLE", "the original result is unavailable", httpserver.RequestID(r.Context()))
			return
		}
		// Deliberately no resume/claim/complete calls, including during SAFE.
		api.WriteJSON(w, http.StatusOK, item)
	}
}
