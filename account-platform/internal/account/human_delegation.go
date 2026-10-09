package account

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/humandelegation"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

type HumanDelegationRequest struct {
	Issuer               string     `json:"issuer"`
	Subject              string     `json:"subject"`
	Audience             string     `json:"audience"`
	Permission           string     `json:"permission"`
	TargetOrganizationID *uuid.UUID `json:"target_organization_id,omitempty"`
	TargetUnitID         *uuid.UUID `json:"target_unit_id,omitempty"`
	TargetResourceType   string     `json:"target_resource_type"`
	TargetResourceID     string     `json:"target_resource_id"`
	HTTPMethod           string     `json:"http_method"`
	HTTPPath             string     `json:"http_path"`
	RequestSHA256        string     `json:"request_sha256"`
}

type HumanDelegationResponse struct {
	Delegation string `json:"delegation"`
	ExpiresIn  int64  `json:"expires_in"`
}

func (s *Service) createHumanDelegation(w http.ResponseWriter, r *http.Request) {
	var request HumanDelegationRequest
	if err := api.DecodeJSON(w, r, &request); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid human delegation request", httpserver.RequestID(r.Context()))
		return
	}
	request.Issuer = strings.TrimSpace(request.Issuer)
	request.Subject = strings.TrimSpace(request.Subject)
	request.Permission = canonicalPermissionCode(request.Permission)
	if request.Issuer == "" || request.Subject == "" || request.Permission == "" || (request.TargetUnitID != nil && request.TargetOrganizationID == nil) {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid human delegation request", httpserver.RequestID(r.Context()))
		return
	}
	if s.humanDelegations == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "HUMAN_DELEGATION_UNAVAILABLE", "human delegation signing is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	repository, ok := s.repository.(authorizationRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "AUTHORIZATION_UNAVAILABLE", "authorization is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	decision, err := repository.Authorize(r.Context(), identity.Principal{Issuer: request.Issuer, Subject: request.Subject}, request.Permission, request.TargetOrganizationID, request.TargetUnitID)
	if err != nil && !errors.Is(err, ErrForbidden) {
		s.writeError(w, r, err)
		return
	}
	if err != nil || !decision.Allowed || decision.UserID == uuid.Nil {
		api.WriteError(w, http.StatusForbidden, "PERMISSION_DENIED", "the requested permission is not granted", httpserver.RequestID(r.Context()))
		return
	}
	token, err := s.humanDelegations.Sign(humandelegation.Claims{
		Audience: request.Audience, HumanUserID: decision.UserID, Permission: request.Permission,
		AuthorizationVersion: decision.AuthorizationVersion, OrganizationID: request.TargetOrganizationID,
		UnitID: request.TargetUnitID, ResourceType: request.TargetResourceType, ResourceID: request.TargetResourceID,
		HTTPMethod: request.HTTPMethod, HTTPPath: request.HTTPPath, RequestSHA256: request.RequestSHA256,
	})
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid human delegation binding", httpserver.RequestID(r.Context()))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeAuthorizationVersions(w.Header(), decision)
	api.WriteJSON(w, http.StatusCreated, HumanDelegationResponse{Delegation: token, ExpiresIn: int64(humandelegation.MaximumLifetime.Seconds())})
}
