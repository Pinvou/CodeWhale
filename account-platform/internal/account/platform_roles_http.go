package account

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"

	"github.com/google/uuid"
)

type platformRoleAdminRepository interface {
	PlatformRoleDefinitions(context.Context) ([]AdminRoleDefinition, error)
	AdminPlatformRoleBindings(context.Context, *uuid.UUID) (RoleBindingList, error)
	AdminGrantPlatformRoleBinding(context.Context, identity.Principal, RoleBindingGrant) (RoleBinding, error)
	AdminRevokePlatformRoleBinding(context.Context, identity.Principal, uuid.UUID) (RoleBinding, error)
}

func (s *Service) registerPlatformRoleRoutes(admin *httpserver.RouteGroup) {
	admin.GET("/roles", s.requirePermission(PermissionPlatformRolesRead), httpserver.AdaptFunc(s.adminPlatformRoles))
	admin.GET("/role-bindings", s.requirePermission(PermissionPlatformRolesRead), httpserver.AdaptFunc(s.adminPlatformRoleBindings))
	admin.POST("/role-bindings", s.requirePermission(PermissionPlatformRolesAssign), approvalpolicy.Require("ACCOUNT_ROLE_BINDING_GRANT", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminGrantPlatformRoleBinding))
	admin.DELETE("/role-bindings/:binding_id", s.requirePermission(PermissionPlatformRolesAssign), approvalpolicy.Require("ACCOUNT_ROLE_BINDING_REVOKE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminRevokePlatformRoleBinding))
}
func (s *Service) platformRoleRepository(w http.ResponseWriter, r *http.Request) (platformRoleAdminRepository, bool) {
	repository, ok := s.repository.(platformRoleAdminRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ROLE_MANAGEMENT_UNAVAILABLE", "role management is unavailable", httpserver.RequestID(r.Context()))
	}
	return repository, ok
}
func (s *Service) adminPlatformRoles(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.platformRoleRepository(w, r)
	if !ok {
		return
	}
	items, err := repository.PlatformRoleDefinitions(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}
func (s *Service) adminPlatformRoleBindings(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.platformRoleRepository(w, r)
	if !ok {
		return
	}
	if r.URL.Query().Has("organization_id") || r.URL.Query().Has("unit_id") {
		s.writeError(w, r, ErrConflict)
		return
	}
	var userID *uuid.UUID
	if raw := strings.TrimSpace(r.URL.Query().Get("user_id")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil || id == uuid.Nil {
			s.writeError(w, r, ErrConflict)
			return
		}
		userID = &id
	}
	result, err := repository.AdminPlatformRoleBindings(r.Context(), userID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, result)
}
func (s *Service) adminGrantPlatformRoleBinding(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.platformRoleRepository(w, r)
	if !ok {
		return
	}
	var body struct {
		UserID         uuid.UUID  `json:"user_id"`
		RoleCode       string     `json:"role_code"`
		OrganizationID *uuid.UUID `json:"organization_id"`
		UnitID         *uuid.UUID `json:"unit_id"`
		StartsAt       *time.Time `json:"starts_at"`
		ExpiresAt      *time.Time `json:"expires_at"`
		IdempotencyKey string     `json:"idempotency_key"`
		Reason         string     `json:"reason"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_JSON", "request body is invalid", httpserver.RequestID(r.Context()))
		return
	}
	item, err := repository.AdminGrantPlatformRoleBinding(r.Context(), mustPrincipal(r.Context()), RoleBindingGrant{UserID: body.UserID, RoleCode: body.RoleCode, OrganizationID: body.OrganizationID, UnitID: body.UnitID, StartsAt: body.StartsAt, ExpiresAt: body.ExpiresAt, IdempotencyKey: body.IdempotencyKey, Reason: body.Reason})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusCreated, item)
}
func (s *Service) adminRevokePlatformRoleBinding(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.platformRoleRepository(w, r)
	if !ok {
		return
	}
	id, ok := pathUUID(w, r, "binding_id")
	if !ok {
		return
	}
	item, err := repository.AdminRevokePlatformRoleBinding(r.Context(), mustPrincipal(r.Context()), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, item)
}
