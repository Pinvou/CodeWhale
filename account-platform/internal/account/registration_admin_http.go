package account

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

func (s *Service) adminRegistrationOperations(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(RegistrationAdminRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "REGISTRATION_REPAIR_UNAVAILABLE", "registration repair is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	olderThanMinutes, _ := strconv.Atoi(r.URL.Query().Get("older_than_minutes"))
	items, err := repository.RegistrationRepairQueue(r.Context(), RegistrationRepairFilter{
		Status: r.URL.Query().Get("status"), Stage: r.URL.Query().Get("stage"),
		Responsibility: r.URL.Query().Get("responsibility"), SLA: r.URL.Query().Get("sla"),
		OlderThanMinutes: olderThanMinutes, Limit: limit,
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) adminRetryRegistration(w http.ResponseWriter, r *http.Request) {
	operationID, ok := registrationOperationPath(w, r)
	if !ok {
		return
	}
	repository, supported := s.repository.(RegistrationAdminRepository)
	if !supported || s.mobileAuth == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "REGISTRATION_REPAIR_UNAVAILABLE", "registration repair is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	principal := mustPrincipal(r.Context())
	operation, err := repository.RegistrationOperation(r.Context(), operationID)
	if err == nil && operation.Status != "REPAIR_REQUIRED" {
		err = ErrRegistrationConflict
	}
	if err == nil {
		if auditErr := repository.AuditRegistrationRepair(r.Context(), principal, operationID, "registration.repair.retry.requested", "SUCCEEDED"); auditErr != nil {
			s.writeRegistrationAdminError(w, r, auditErr)
			return
		}
	}
	if err == nil && operation.UserID != nil {
		err = repository.RetryWalletProvisioning(r.Context(), principal, *operation.UserID)
	} else if err == nil {
		_, err = s.mobileAuth.resumeRegistration(r.Context(), operation)
		if errors.Is(err, ErrRegistrationRecovering) {
			err = nil
		}
	}
	result := "SUCCEEDED"
	if err != nil {
		result = "FAILED"
	}
	if auditErr := repository.AuditRegistrationRepair(r.Context(), principal, operationID, "registration.repair.retry", result); auditErr != nil {
		signalRegistrationRepairAuditPending(w, r)
	}
	if err != nil {
		s.writeRegistrationAdminError(w, r, err)
		return
	}
	operation, err = repository.RegistrationOperation(r.Context(), operationID)
	if err != nil {
		// The repair call returned, but its authoritative operation projection
		// is unavailable. Do not return an empty operation or invite a new
		// command based on a fabricated result.
		api.WriteError(w, http.StatusServiceUnavailable, "REGISTRATION_REPAIR_RESULT_UNKNOWN", "the repair result is unavailable; recover the original operation", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteJSON(w, http.StatusAccepted, registrationResult(operation))
}

func (s *Service) adminCompensateRegistration(w http.ResponseWriter, r *http.Request) {
	operationID, ok := registrationOperationPath(w, r)
	if !ok {
		return
	}
	repository, supported := s.repository.(RegistrationAdminRepository)
	if !supported || s.mobileAuth == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "REGISTRATION_REPAIR_UNAVAILABLE", "registration repair is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	principal := mustPrincipal(r.Context())
	if err := repository.AuditRegistrationRepair(r.Context(), principal, operationID, "registration.repair.compensate.requested", "SUCCEEDED"); err != nil {
		s.writeRegistrationAdminError(w, r, err)
		return
	}
	operation, err := repository.PrepareRegistrationCompensation(r.Context(), principal, operationID)
	if err == nil {
		err = s.mobileAuth.identities.DisableUserAndConfirm(r.Context(), operation.IdentitySubject)
	}
	if err == nil {
		err = repository.CompleteRegistrationCompensation(r.Context(), principal, operationID)
	}
	if err != nil {
		if auditErr := repository.AuditRegistrationRepair(r.Context(), principal, operationID, "registration.repair.compensate", "FAILED"); auditErr != nil {
			signalRegistrationRepairAuditPending(w, r)
		}
		s.writeRegistrationAdminError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func registrationOperationPath(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	operationID, err := uuid.Parse(strings.TrimSpace(r.PathValue("operation_id")))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid registration operation", httpserver.RequestID(r.Context()))
		return uuid.Nil, false
	}
	return operationID, true
}

func (s *Service) writeRegistrationAdminError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrRegistrationCompensationForbidden) {
		api.WriteError(w, http.StatusConflict, "REGISTRATION_COMPENSATION_FORBIDDEN", "the registration has not satisfied the compensation proof gate", httpserver.RequestID(r.Context()))
		return
	}
	if errors.Is(err, ErrNotFound) {
		api.WriteError(w, http.StatusNotFound, "REGISTRATION_OPERATION_NOT_FOUND", "the registration operation was not found", httpserver.RequestID(r.Context()))
		return
	}
	s.writeError(w, r, err)
}

// The requested audit is persisted before effects. A missing terminal audit
// does not turn a known business result into a new dispatchable command.
func signalRegistrationRepairAuditPending(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Registration-Repair-Audit", "pending")
	slog.ErrorContext(r.Context(), "REGISTRATION_REPAIR_AUDIT_PENDING", "request_id", httpserver.RequestID(r.Context()))
}
