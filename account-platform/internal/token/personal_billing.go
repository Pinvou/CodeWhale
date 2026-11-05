package token

import (
	"errors"

	"net/http"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
)

func (s *Service) resolvePersonalBilling(w http.ResponseWriter, r *http.Request, permission string) (AuthorizationContext, bool) {
	if s.authorizer == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "AUTHORIZATION_UNAVAILABLE", "authorization is temporarily unavailable", httpserver.RequestID(r.Context()))
		return AuthorizationContext{}, false
	}
	principal, ok := identity.FromContext(r.Context())
	if !ok {
		api.WriteError(w, http.StatusUnauthorized, "AUTH_REQUIRED", "a valid bearer token is required", httpserver.RequestID(r.Context()))
		return AuthorizationContext{}, false
	}
	userID, err := s.authorizer.AuthorizedUserID(r.Context(), principal, permission)
	if errors.Is(err, authorization.ErrPermissionDenied) {
		s.writeError(w, r, ErrForbidden)
		return AuthorizationContext{}, false
	}
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "AUTHORIZATION_UNAVAILABLE", "authorization is temporarily unavailable", httpserver.RequestID(r.Context()))
		return AuthorizationContext{}, false
	}
	resolved, ok := s.resolveContext(w, r)
	if !ok {
		return AuthorizationContext{}, false
	}
	if resolved.UserID != userID || resolved.ContextType != "PERSONAL" || resolved.OrganizationID != nil {
		s.writeError(w, r, ErrForbidden)
		return AuthorizationContext{}, false
	}
	return resolved, true
}
