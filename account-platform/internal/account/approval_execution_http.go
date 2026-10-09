package account

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

type approvalExecutionRepository interface {
	BeginApprovalExecution(context.Context, uuid.UUID, string, string, json.RawMessage, json.RawMessage) (ApprovalExecution, error)
	CompleteApprovalExecution(context.Context, uuid.UUID, string, string, string, json.RawMessage, json.RawMessage, string) error
}

type approvalAuthorizedExecutionRepository interface {
	BeginApprovalExecutionWithAuthorization(context.Context, uuid.UUID, string, string, json.RawMessage, json.RawMessage, ApprovalAuthorizationBinding) (ApprovalExecution, error)
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
	executorIdentity, ok := approvalExecutorFromContext(w, r)
	if !ok {
		return
	}
	var body struct {
		JobRunID                  string                        `json:"job_run_id"`
		ObservedVersions          json.RawMessage               `json:"observed_versions"`
		ObservedFinancialSnapshot json.RawMessage               `json:"observed_financial_snapshot"`
		AuthorizationBinding      *ApprovalAuthorizationBinding `json:"authorization_binding,omitempty"`
	}
	if api.DecodeJSON(w, r, &body) != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_EXECUTION", "invalid approval execution request", httpserver.RequestID(r.Context()))
		return
	}
	var execution ApprovalExecution
	var err error
	if body.AuthorizationBinding != nil {
		authorized, supported := repository.(approvalAuthorizedExecutionRepository)
		if !supported {
			err = ErrApprovalConsumerBindingUnavailable
		} else {
			execution, err = authorized.BeginApprovalExecutionWithAuthorization(r.Context(), requestID, executorIdentity, body.JobRunID, body.ObservedVersions, body.ObservedFinancialSnapshot, *body.AuthorizationBinding)
		}
	} else {
		execution, err = repository.BeginApprovalExecution(r.Context(), requestID, executorIdentity, body.JobRunID, body.ObservedVersions, body.ObservedFinancialSnapshot)
	}
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
	executorIdentity, ok := approvalExecutorFromContext(w, r)
	if !ok {
		return
	}
	var body struct {
		JobRunID         string          `json:"job_run_id"`
		Result           string          `json:"result"`
		DomainResult     json.RawMessage `json:"domain_result"`
		LedgerReferences json.RawMessage `json:"ledger_references"`
		ErrorClass       string          `json:"error_class"`
	}
	if api.DecodeJSON(w, r, &body) != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_EXECUTION", "invalid approval execution completion", httpserver.RequestID(r.Context()))
		return
	}
	if err := repository.CompleteApprovalExecution(r.Context(), requestID, executorIdentity, body.JobRunID, body.Result, body.DomainResult, body.LedgerReferences, body.ErrorClass); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func approvalExecutorFromContext(w http.ResponseWriter, r *http.Request) (string, bool) {
	workload, ok := serviceauth.FromContext(r.Context())
	if !ok || workload.ActorType != "SERVICE" || strings.TrimSpace(workload.Service) == "" || strings.TrimSpace(workload.Subject) == "" {
		api.WriteError(w, http.StatusForbidden, "SERVICE_AUTH_FORBIDDEN", "trusted executor identity is required", httpserver.RequestID(r.Context()))
		return "", false
	}
	service := strings.TrimSpace(workload.Service)
	if service == "billing-service" {
		return "billing-command-executor", true
	}
	return service, true
}
