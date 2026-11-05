package billing

import (
	"context"
	"errors"
	"net/http"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/usagewaiver"
	"github.com/google/uuid"
)

type UsageWaiverTokenClient interface {
	WaiveUsage(context.Context, usagewaiver.Command) (usagewaiver.Decision, error)
}
type HTTPUsageWaiverTokenClient struct{ client *usagewaiver.Client }

func NewHTTPUsageWaiverTokenClient(baseURL, credential string, client *http.Client) (*HTTPUsageWaiverTokenClient, error) {
	c, err := usagewaiver.NewClient(baseURL, credential, client)
	if err != nil {
		return nil, err
	}
	return &HTTPUsageWaiverTokenClient{client: c}, nil
}
func (c *HTTPUsageWaiverTokenClient) WaiveUsage(ctx context.Context, command usagewaiver.Command) (usagewaiver.Decision, error) {
	var result usagewaiver.Decision
	err := c.client.Call(ctx, "/internal/v1/usage-waivers", command, &result)
	if err == nil && (!result.ValidFor(command) || (result.Admission == usagewaiver.Admitted && result.Result == nil)) {
		err = ErrConflict
	}
	return result, err
}

type usageWaiverRepository interface {
	RecordUsageWaiverRejection(context.Context, usagewaiver.Command, usagewaiver.Decision) error
	PrepareUsageWaiver(context.Context, uuid.UUID, string, string, UsageWaiverActor) (usagewaiver.Command, error)
	ExecuteUsageWaiver(context.Context, usagewaiver.ExecuteRequest) (usagewaiver.Result, error)
	LegacyUsageWaiver(context.Context, uuid.UUID, string, string, UsageWaiverActor) (LegacyUsageWaiverRecord, error)
}

func (s *Service) WithUsageWaiverClient(client UsageWaiverTokenClient) *Service {
	s.usageWaiverClient = client
	return s
}

func (s *Service) adminUsageWaiver(w http.ResponseWriter, r *http.Request) {
	store, ok := s.repository.(usageWaiverRepository)
	if !ok || s.usageWaiverClient == nil || s.authorizer == nil {
		api.WriteError(w, 503, "USAGE_WAIVER_UNAVAILABLE", "usage waiver is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	holdID, err := uuid.Parse(r.PathValue("hold_id"))
	if err != nil || holdID == uuid.Nil {
		api.WriteError(w, 422, "INVALID_USAGE_WAIVER", "hold is invalid", httpserver.RequestID(r.Context()))
		return
	}
	var input struct {
		Reason string `json:"reason"`
	}
	if api.DecodeJSON(w, r, &input) != nil {
		api.WriteError(w, 400, "INVALID_JSON", "request body is invalid", httpserver.RequestID(r.Context()))
		return
	}
	principal, ok := identity.FromContext(r.Context())
	if !ok {
		api.WriteError(w, 401, "UNAUTHENTICATED", "authentication required", httpserver.RequestID(r.Context()))
		return
	}
	actorID, version, err := s.authorizer.AuthorizedUserIDAll(r.Context(), principal, "platform.billing.holds.release")
	if err != nil {
		status, code := 503, "AUTHORIZATION_UNAVAILABLE"
		if errors.Is(err, authorization.ErrPermissionDenied) {
			status, code = 403, "PERMISSION_DENIED"
		}
		api.WriteError(w, status, code, "usage waiver authorization unavailable", httpserver.RequestID(r.Context()))
		return
	}
	command, err := store.PrepareUsageWaiver(r.Context(), holdID, input.Reason, r.Header.Get("Idempotency-Key"), UsageWaiverActor{ID: actorID, Issuer: principal.Issuer, Subject: principal.Subject, AuthorizationVersion: version})
	if errors.Is(err, ErrLegacyHoldWaiverPending) {
		api.WriteError(w, 409, "LEGACY_WAIVER_REQUIRES_REVIEW", "the original legacy waiver on this hold requires review", httpserver.RequestID(r.Context()))
		return
	}
	if errors.Is(err, ErrLegacyWaiverRequiresReview) {
		record, readErr := store.LegacyUsageWaiver(r.Context(), holdID, input.Reason, r.Header.Get("Idempotency-Key"), UsageWaiverActor{ID: actorID, Issuer: principal.Issuer, Subject: principal.Subject, AuthorizationVersion: version})
		if readErr != nil {
			s.writeError(w, r, readErr)
			return
		}
		if record.Completed {
			writeAdminFinancialJSON(w, 200, record)
		} else {
			api.WriteJSON(w, 409, api.Error{Code: "LEGACY_WAIVER_REQUIRES_REVIEW", Message: "the original legacy waiver requires review", RequestID: httpserver.RequestID(r.Context()), Details: map[string]any{"legacy_command": record}})
		}
		return
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	result, err := s.usageWaiverClient.WaiveUsage(r.Context(), command)
	if err != nil {
		api.WriteError(w, 503, "USAGE_WAIVER_RESULT_UNKNOWN", "retry the original waiver key for recovery", httpserver.RequestID(r.Context()))
		return
	}
	if result.Admission == usagewaiver.Rejected {
		if err := store.RecordUsageWaiverRejection(r.Context(), command, result); err != nil {
			s.writeError(w, r, err)
			return
		}
		api.WriteJSON(w, 409, api.Error{Code: "USAGE_WAIVER_REJECTED", Message: "the original waiver was definitively rejected", RequestID: httpserver.RequestID(r.Context()), Details: map[string]any{"decision": result}})
		return
	}
	writeAdminFinancialJSON(w, 200, result.Result)
}

func (s *Service) executeUsageWaiver(w http.ResponseWriter, r *http.Request) {
	store, ok := s.repository.(usageWaiverRepository)
	if !ok {
		api.WriteError(w, 503, "USAGE_WAIVER_UNAVAILABLE", "usage waiver is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	var input usagewaiver.ExecuteRequest
	if api.DecodeJSON(w, r, &input) != nil {
		api.WriteError(w, 400, "INVALID_JSON", "request body is invalid", httpserver.RequestID(r.Context()))
		return
	}
	result, err := store.ExecuteUsageWaiver(r.Context(), input)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, 200, result)
}
