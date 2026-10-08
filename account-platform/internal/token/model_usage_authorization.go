package token

import (
	"errors"
	"net/http"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

func (s *Service) resolveModelUsagePermission(w http.ResponseWriter, r *http.Request, personalPermission, organizationPermission string) (AuthorizationContext, bool) {
	if s.authorizer == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "AUTHORIZATION_UNAVAILABLE", "authorization is temporarily unavailable", httpserver.RequestID(r.Context()))
		return AuthorizationContext{}, false
	}
	principal, ok := identity.FromContext(r.Context())
	if !ok {
		api.WriteError(w, http.StatusUnauthorized, "AUTH_REQUIRED", "a valid bearer token is required", httpserver.RequestID(r.Context()))
		return AuthorizationContext{}, false
	}
	resolved, ok := s.resolveContext(w, r)
	if !ok {
		return AuthorizationContext{}, false
	}
	var authorizedUserID uuid.UUID
	var err error
	switch resolved.ContextType {
	case "PERSONAL":
		if resolved.OrganizationID != nil || personalPermission == "" {
			s.writeError(w, r, ErrForbidden)
			return AuthorizationContext{}, false
		}
		authorizedUserID, err = s.authorizer.AuthorizedUserID(r.Context(), principal, personalPermission)
	case "ORGANIZATION":
		if resolved.OrganizationID == nil || *resolved.OrganizationID == uuid.Nil || organizationPermission == "" || resolved.OrganizationMembershipVersion == nil || *resolved.OrganizationMembershipVersion < 1 {
			s.writeError(w, r, ErrForbidden)
			return AuthorizationContext{}, false
		}
		var version int64
		authorizedUserID, version, err = s.authorizer.AuthorizedOrganizationUserID(r.Context(), principal, organizationPermission, *resolved.OrganizationID)
		if err == nil {
			resolved.AuthorizationVersion = version
		}
	default:
		s.writeError(w, r, ErrForbidden)
		return AuthorizationContext{}, false
	}
	if errors.Is(err, authorization.ErrPermissionDenied) {
		s.writeError(w, r, ErrForbidden)
		return AuthorizationContext{}, false
	}
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "AUTHORIZATION_UNAVAILABLE", "authorization is temporarily unavailable", httpserver.RequestID(r.Context()))
		return AuthorizationContext{}, false
	}
	if authorizedUserID == uuid.Nil || authorizedUserID != resolved.UserID {
		s.writeError(w, r, ErrForbidden)
		return AuthorizationContext{}, false
	}
	return resolved, true
}

func (s *Service) authorizePersonalUsageUser(w http.ResponseWriter, r *http.Request, permission string) (uuid.UUID, bool) {
	if s.authorizer == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "AUTHORIZATION_UNAVAILABLE", "authorization is temporarily unavailable", httpserver.RequestID(r.Context()))
		return uuid.Nil, false
	}
	principal, ok := identity.FromContext(r.Context())
	if !ok {
		api.WriteError(w, http.StatusUnauthorized, "AUTH_REQUIRED", "a valid bearer token is required", httpserver.RequestID(r.Context()))
		return uuid.Nil, false
	}
	userID, err := s.authorizer.AuthorizedUserID(r.Context(), principal, permission)
	if errors.Is(err, authorization.ErrPermissionDenied) {
		s.writeError(w, r, ErrForbidden)
		return uuid.Nil, false
	}
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "AUTHORIZATION_UNAVAILABLE", "authorization is temporarily unavailable", httpserver.RequestID(r.Context()))
		return uuid.Nil, false
	}
	if userID == uuid.Nil {
		s.writeError(w, r, ErrForbidden)
		return uuid.Nil, false
	}
	return userID, true
}
