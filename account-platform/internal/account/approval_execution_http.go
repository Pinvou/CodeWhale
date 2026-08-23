package account

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

type approvalExecutionRepository interface {
	BeginApprovalExecution(context.Context, uuid.UUID, string, string, json.RawMessage) (ApprovalExecution, error)
	CompleteApprovalExecution(context.Context, uuid.UUID, string, string, string, json.RawMessage, json.RawMessage, string) error
}

func (s *Service) requireApprovalExecutionRepository(w http.ResponseWriter, r *http.Request) (approvalExecutionRepository, bool) {
	repository, ok := s.repository.(approvalExecutionRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_EXECUTION_UNAVAILABLE", "approval execution is unavailable", httpserver.RequestID(r.Context()))
	}
	return repository, ok
}

func (s *Service) beginApprovalExecution(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireApprovalExecutionRepository(w, r)
	if !ok {
		return
	}
	requestID, ok := pathUUID(w, r, "approval_request_id")
	if !ok {
		return
	}
	var body struct {
		ExecutorIdentity string          `json:"executor_identity"`
		JobRunID         string          `json:"job_run_id"`
		ObservedVersions json.RawMessage `json:"observed_versions"`
	}
	if api.DecodeJSON(w, r, &body) != nil {
		return
	}
	execution, err := repository.BeginApprovalExecution(r.Context(), requestID, body.ExecutorIdentity, body.JobRunID, body.ObservedVersions)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, execution)
}

func (s *Service) completeApprovalExecution(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireApprovalExecutionRepository(w, r)
	if !ok {
		return
	}
	requestID, ok := pathUUID(w, r, "approval_request_id")
	if !ok {
		return
	}
	var body struct {
		ExecutorIdentity string          `json:"executor_identity"`
		JobRunID         string          `json:"job_run_id"`
		Result           string          `json:"result"`
		DomainResult     json.RawMessage `json:"domain_result"`
		LedgerReferences json.RawMessage `json:"ledger_references"`
		ErrorClass       string          `json:"error_class"`
	}
	if api.DecodeJSON(w, r, &body) != nil {
		return
	}
	if err := repository.CompleteApprovalExecution(r.Context(), requestID, body.ExecutorIdentity, body.JobRunID, body.Result, body.DomainResult, body.LedgerReferences, body.ErrorClass); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
