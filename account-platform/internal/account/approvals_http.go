package account

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

type approvalRepository interface {
	ApprovalPolicies(context.Context) []approvalpolicy.Policy
	ApprovalDirectory(context.Context, identity.Principal) (ApprovalDirectoryProjection, error)
	CreateApprovalDraft(context.Context, identity.Principal, ApprovalDraft) (ApprovalRequest, error)
	UpdateApprovalDraft(context.Context, identity.Principal, uuid.UUID, int64, ApprovalDraft) (ApprovalRequest, error)
	SubmitApprovalRequest(context.Context, identity.Principal, uuid.UUID, int64) (ApprovalRequest, error)
	WithdrawApprovalRequest(context.Context, identity.Principal, uuid.UUID, int64) (ApprovalRequest, error)
	DecideApprovalRequest(context.Context, identity.Principal, uuid.UUID, int64, string, string, json.RawMessage) (ApprovalRequest, error)
	ListApprovalRequests(context.Context, identity.Principal, ApprovalFilter) ([]ApprovalRequest, error)
	ApprovalRequest(context.Context, identity.Principal, uuid.UUID) (ApprovalRequest, error)
}

func (s *Service) adminApprovalDirectory(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireApprovalRepository(w, r)
	if !ok {
		return
	}
	projection, err := repository.ApprovalDirectory(r.Context(), mustPrincipal(r.Context()))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("X-Platform-Authorization-Version", stringInt64(projection.PlatformAuthorizationVersion))
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, projection)
}

func (s *Service) requireApprovalRepository(w http.ResponseWriter, r *http.Request) (approvalRepository, bool) {
	repository, ok := s.repository.(approvalRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_UNAVAILABLE", "approval workflow is unavailable", httpserver.RequestID(r.Context()))
	}
	return repository, ok
}

type approvalDraftBody struct {
	CommandType              string          `json:"command_type"`
	IdempotencyKey           string          `json:"idempotency_key"`
	NormalizedCommand        json.RawMessage `json:"normalized_command"`
	TargetResourceType       string          `json:"target_resource_type"`
	TargetResourceID         string          `json:"target_resource_id"`
	Reason                   string          `json:"reason"`
	RedactedEvidence         json.RawMessage `json:"redacted_evidence"`
	ExpectedImpact           json.RawMessage `json:"expected_impact"`
	CriticalResourceVersions json.RawMessage `json:"critical_resource_versions"`
	FinancialSnapshot        json.RawMessage `json:"financial_snapshot"`
	SupersedesRequestID      *uuid.UUID      `json:"supersedes_request_id"`
	ExpectedVersion          int64           `json:"expected_version,omitempty"`
}

func (body approvalDraftBody) model() ApprovalDraft {
	return ApprovalDraft{CommandType: body.CommandType, IdempotencyKey: body.IdempotencyKey, NormalizedCommand: body.NormalizedCommand, TargetResourceType: body.TargetResourceType, TargetResourceID: body.TargetResourceID, Reason: body.Reason, RedactedEvidence: body.RedactedEvidence, ExpectedImpact: body.ExpectedImpact, CriticalResourceVersions: body.CriticalResourceVersions, FinancialSnapshot: body.FinancialSnapshot, SupersedesRequestID: body.SupersedesRequestID}
}

func (s *Service) adminApprovalPolicies(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireApprovalRepository(w, r)
	if !ok {
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": repository.ApprovalPolicies(r.Context()), "policy_version": 1})
}

func (s *Service) adminApprovalRequests(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireApprovalRepository(w, r)
	if !ok {
		return
	}
	filter, err := approvalFilterFromRequest(r)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_FILTER", "approval filter is invalid", httpserver.RequestID(r.Context()))
		return
	}
	items, err := repository.ListApprovalRequests(r.Context(), mustPrincipal(r.Context()), filter)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

type approvalV2ListRepository interface {
	ListApprovalRequestsV2(context.Context, identity.Principal, ApprovalFilter, string, int, string) (ApprovalV2Page, error)
}

func (s *Service) adminApprovalRequestsV2(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	repository, ok := s.repository.(approvalV2ListRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "PAGINATION_SNAPSHOT_UNAVAILABLE", "approval pagination is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	query := r.URL.Query()
	for name, values := range query {
		switch name {
		case "command_type", "status", "risk_class", "requester_user_id", "target", "submitted_from", "submitted_to", "expires_from", "expires_to", "limit", "cursor":
		default:
			api.WriteError(w, http.StatusBadRequest, "INVALID_FILTER", "approval filter is invalid", httpserver.RequestID(r.Context()))
			return
		}
		if len(values) != 1 {
			api.WriteError(w, http.StatusBadRequest, "INVALID_FILTER", "approval filter is invalid", httpserver.RequestID(r.Context()))
			return
		}
	}
	filter, err := approvalFilterFromRequest(r)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_FILTER", "approval filter is invalid", httpserver.RequestID(r.Context()))
		return
	}
	limit := 0
	if raw := query.Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || raw != strconv.Itoa(limit) || limit < 1 || limit > 200 {
			api.WriteError(w, http.StatusBadRequest, "INVALID_LIMIT", "approval page limit is invalid", httpserver.RequestID(r.Context()))
			return
		}
	}
	filter.Limit = 0
	sourceIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || net.ParseIP(sourceIP) == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "PAGINATION_SNAPSHOT_UNAVAILABLE", "approval pagination is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	page, err := repository.ListApprovalRequestsV2(r.Context(), mustPrincipal(r.Context()), filter, query.Get("cursor"), limit, sourceIP)
	if err != nil {
		if errors.Is(err, ErrForbidden) {
			s.writeError(w, r, err)
			return
		}
		classification := ClassifyApprovalPaginationError(err)
		if classification.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(classification.RetryAfter))
		}
		api.WriteError(w, classification.Status, classification.Code, "approval pagination is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteJSON(w, http.StatusOK, page)
}

func approvalFilterFromRequest(r *http.Request) (ApprovalFilter, error) {
	query := r.URL.Query()
	filter := ApprovalFilter{CommandType: query.Get("command_type"), Status: query.Get("status"), RiskClass: query.Get("risk_class"), Target: query.Get("target"), Limit: adminLimit(r)}
	if value := strings.TrimSpace(query.Get("requester_user_id")); value != "" {
		parsed, err := uuid.Parse(value)
		if err != nil {
			return ApprovalFilter{}, err
		}
		filter.Requester = &parsed
	}
	for name, destination := range map[string]**time.Time{
		"submitted_from": &filter.SubmittedFrom, "submitted_to": &filter.SubmittedTo,
		"expires_from": &filter.ExpiresFrom, "expires_to": &filter.ExpiresTo,
	} {
		value := strings.TrimSpace(query.Get(name))
		if value == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return ApprovalFilter{}, err
		}
		*destination = &parsed
	}
	return filter, nil
}

func (s *Service) adminCreateApprovalDraft(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireApprovalRepository(w, r)
	if !ok {
		return
	}
	var body approvalDraftBody
	if api.DecodeJSON(w, r, &body) != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_REQUEST", "invalid approval request", httpserver.RequestID(r.Context()))
		return
	}
	item, err := repository.CreateApprovalDraft(r.Context(), mustPrincipal(r.Context()), body.model())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusCreated, item)
}

func (s *Service) adminUpdateApprovalDraft(w http.ResponseWriter, r *http.Request) {
	repository, requestID, ok := s.approvalRequestTarget(w, r)
	if !ok {
		return
	}
	var body approvalDraftBody
	if api.DecodeJSON(w, r, &body) != nil || body.ExpectedVersion <= 0 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_REQUEST", "invalid approval request", httpserver.RequestID(r.Context()))
		return
	}
	item, err := repository.UpdateApprovalDraft(r.Context(), mustPrincipal(r.Context()), requestID, body.ExpectedVersion, body.model())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, item)
}

func (s *Service) adminApprovalRequest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	repository, requestID, ok := s.approvalRequestTarget(w, r)
	if !ok {
		return
	}
	item, err := repository.ApprovalRequest(r.Context(), mustPrincipal(r.Context()), requestID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	reader, ok := s.repository.(interface {
		ApprovalActionControl(context.Context) (AdminApprovalControl, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_UNAVAILABLE", "approval workflow is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	control, err := reader.ApprovalActionControl(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	item.AllowedActions, err = approvalActionsForControl(item.AllowedActions, control)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("X-Pinvou-Approval-Action-Control-Version", strconv.FormatInt(control.Version, 10))
	api.WriteJSON(w, http.StatusOK, item)
}

func (s *Service) adminApprovalRequestV2(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	id, ok := pathUUID(w, r, "approval_request_id")
	if !ok {
		return
	}
	repository, ok := s.repository.(interface {
		ApprovalRequestV2(context.Context, identity.Principal, uuid.UUID) (any, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_UNAVAILABLE", "approval workflow is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	detail, err := repository.ApprovalRequestV2(r.Context(), mustPrincipal(r.Context()), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, detail)
}

func (s *Service) adminSubmitApprovalRequest(w http.ResponseWriter, r *http.Request) {
	s.adminTransitionApproval(w, r, "submit")
}

func (s *Service) adminWithdrawApprovalRequest(w http.ResponseWriter, r *http.Request) {
	s.adminTransitionApproval(w, r, "withdraw")
}

func (s *Service) adminTransitionApproval(w http.ResponseWriter, r *http.Request, action string) {
	repository, requestID, ok := s.approvalRequestTarget(w, r)
	if !ok {
		return
	}
	var body struct {
		ExpectedVersion int64 `json:"expected_version"`
	}
	if api.DecodeJSON(w, r, &body) != nil || body.ExpectedVersion <= 0 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_REQUEST", "invalid approval request", httpserver.RequestID(r.Context()))
		return
	}
	principal := mustPrincipal(r.Context())
	var item ApprovalRequest
	var err error
	if action == "submit" {
		item, err = repository.SubmitApprovalRequest(r.Context(), principal, requestID, body.ExpectedVersion)
	} else {
		item, err = repository.WithdrawApprovalRequest(r.Context(), principal, requestID, body.ExpectedVersion)
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, item)
}

func (s *Service) adminDecideApprovalRequest(w http.ResponseWriter, r *http.Request) {
	repository, requestID, ok := s.approvalRequestTarget(w, r)
	if !ok {
		return
	}
	var body struct {
		ExpectedVersion  int64           `json:"expected_version"`
		Decision         string          `json:"decision"`
		Reason           string          `json:"reason"`
		ObservedVersions json.RawMessage `json:"observed_versions"`
	}
	if api.DecodeJSON(w, r, &body) != nil || body.ExpectedVersion <= 0 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_REQUEST", "invalid approval request", httpserver.RequestID(r.Context()))
		return
	}
	item, err := repository.DecideApprovalRequest(r.Context(), mustPrincipal(r.Context()), requestID, body.ExpectedVersion, body.Decision, body.Reason, body.ObservedVersions)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, item)
}

func (s *Service) adminDecideApprovalRequestV2(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	repository, requestID, ok := s.approvalRequestTarget(w, r)
	if !ok {
		return
	}
	view, ok := s.repository.(interface {
		ApprovalRequestV2(context.Context, identity.Principal, uuid.UUID) (any, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_UNAVAILABLE", "approval workflow is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	decision, err := decodeApprovalV2Decision(r.Body)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_DECISION", "invalid approval decision", httpserver.RequestID(r.Context()))
		return
	}
	if decision.Decision == "APPROVED" {
		// A v2 body intentionally has no observed_versions. Do not let it
		// enter the legacy comparison, where nil would invalidate a request.
		// Check visibility before reading the gate, as for all decision reads.
		if _, err := repository.ApprovalRequest(r.Context(), mustPrincipal(r.Context()), requestID); err != nil {
			s.writeError(w, r, err)
			return
		}
		gate, ok := s.repository.(interface {
			ApprovalActionControl(context.Context) (AdminApprovalControl, error)
		})
		if !ok {
			s.writeError(w, r, ErrApprovalProofUnavailable)
			return
		}
		control, err := gate.ApprovalActionControl(r.Context())
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		if control.Version != decision.ActionControlVersion {
			s.writeError(w, r, ErrApprovalActionControlChanged)
			return
		}
		if control.Mode != AdminApprovalModeProofRequired {
			s.writeError(w, r, ErrApprovalProofUnavailable)
			return
		}
	}
	decisionCtx := context.WithValue(r.Context(), approvalV2ControlVersionKey{}, decision.ActionControlVersion)
	if _, err := repository.DecideApprovalRequest(decisionCtx, mustPrincipal(r.Context()), requestID, decision.ExpectedVersion, decision.Decision, decision.Reason, nil); err != nil {
		s.writeError(w, r, err)
		return
	}
	detail, err := view.ApprovalRequestV2(r.Context(), mustPrincipal(r.Context()), requestID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, detail)
}

func (s *Service) approvalRequestTarget(w http.ResponseWriter, r *http.Request) (approvalRepository, uuid.UUID, bool) {
	repository, ok := s.requireApprovalRepository(w, r)
	if !ok {
		return nil, uuid.Nil, false
	}
	requestID, ok := pathUUID(w, r, "approval_request_id")
	return repository, requestID, ok
}
