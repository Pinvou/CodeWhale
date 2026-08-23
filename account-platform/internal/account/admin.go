package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type AdminOverview struct {
	Users              int64 `json:"users"`
	ActiveUsers        int64 `json:"active_users"`
	DisabledUsers      int64 `json:"disabled_users"`
	Organizations      int64 `json:"organizations"`
	ActiveMemberships  int64 `json:"active_memberships"`
	PendingInvitations int64 `json:"pending_invitations"`
}

type AdminUser struct {
	ID                uuid.UUID  `json:"user_id"`
	UserCode          string     `json:"user_code"`
	Username          *string    `json:"username,omitempty"`
	DisplayName       *string    `json:"display_name,omitempty"`
	Email             *string    `json:"email,omitempty"`
	Status            string     `json:"status"`
	OrganizationCount int64      `json:"organization_count"`
	LastSeenAt        *time.Time `json:"last_seen_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

type AdminOrganization struct {
	ID          uuid.UUID `json:"organization_id"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	MemberCount int64     `json:"member_count"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type AdminRoleDefinition struct {
	Code        string   `json:"code"`
	Scope       string   `json:"scope"`
	Assignable  bool     `json:"assignable"`
	Description string   `json:"description"`
	Permissions []string `json:"permissions"`
}

type AdminUserMembership struct {
	OrganizationID   uuid.UUID   `json:"organization_id"`
	OrganizationName string      `json:"organization_name"`
	MembershipStatus string      `json:"membership_status"`
	Roles            []string    `json:"roles"`
	UnitAdminIDs     []uuid.UUID `json:"unit_admin_unit_ids"`
}

type AdminAuditLog struct {
	ID                   uuid.UUID       `json:"audit_id"`
	ActorUserID          *uuid.UUID      `json:"actor_user_id,omitempty"`
	ActorDisplayName     *string         `json:"actor_display_name,omitempty"`
	ActorEmail           *string         `json:"actor_email,omitempty"`
	ActiveOrganizationID *uuid.UUID      `json:"active_organization_id,omitempty"`
	Action               string          `json:"action"`
	ResourceType         string          `json:"resource_type"`
	ResourceID           string          `json:"resource_id"`
	Result               string          `json:"result"`
	RequestID            string          `json:"request_id"`
	ChangeSummary        json.RawMessage `json:"change_summary"`
	OccurredAt           time.Time       `json:"occurred_at"`
}

type AdminAuditFilter struct {
	Actor    string
	Action   string
	Resource string
	Result   string
	From     *time.Time
	To       *time.Time
	Limit    int
}

type adminRepository interface {
	AdminOverview(context.Context) (AdminOverview, error)
	AdminRoleBindings(context.Context, *uuid.UUID, *uuid.UUID) ([]RoleBinding, error)
	AdminGrantRoleBinding(context.Context, identity.Principal, RoleBindingGrant) (RoleBinding, error)
	AdminRevokeRoleBinding(context.Context, identity.Principal, uuid.UUID) (RoleBinding, error)
	AdminUsers(context.Context, string, int) ([]AdminUser, error)
	AdminUserMemberships(context.Context, uuid.UUID) ([]AdminUserMembership, error)
	AdminSetUserStatus(context.Context, identity.Principal, uuid.UUID, string) error
	AdminDeleteUser(context.Context, identity.Principal, uuid.UUID) error
	AdminOrganizations(context.Context, string, int) ([]AdminOrganization, error)
	AdminCreateOrganization(context.Context, identity.Principal, uuid.UUID, string) (Organization, error)
	AdminRenameOrganization(context.Context, identity.Principal, uuid.UUID, string) error
	AdminSetOrganizationStatus(context.Context, identity.Principal, uuid.UUID, string) error
	AdminDeleteOrganization(context.Context, identity.Principal, uuid.UUID) error
	AdminUnits(context.Context, uuid.UUID) ([]Unit, error)
	AdminMembers(context.Context, uuid.UUID) ([]Member, error)
	AdminAddMember(context.Context, identity.Principal, uuid.UUID, uuid.UUID, []string, []uuid.UUID) (Member, error)
	AdminReplaceMemberRoles(context.Context, identity.Principal, uuid.UUID, uuid.UUID, []string, []uuid.UUID) (Member, error)
	AdminRemoveMember(context.Context, identity.Principal, uuid.UUID, uuid.UUID) error
	AdminTransferOwnership(context.Context, identity.Principal, uuid.UUID, uuid.UUID) error
	AdminCreateUnit(context.Context, identity.Principal, uuid.UUID, *uuid.UUID, string) (Unit, error)
	AdminRenameUnit(context.Context, identity.Principal, uuid.UUID, uuid.UUID, string) error
	AdminDeleteUnit(context.Context, identity.Principal, uuid.UUID, uuid.UUID) error
	AdminAuditLogs(context.Context, AdminAuditFilter) ([]AdminAuditLog, error)
	AdminAuditLog(context.Context, uuid.UUID) (AdminAuditLog, error)
}

func (s *Service) requireAdminRepository(w http.ResponseWriter, r *http.Request) (adminRepository, bool) {
	repository, ok := s.repository.(adminRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ADMIN_UNAVAILABLE", "account administration is unavailable", httpserver.RequestID(r.Context()))
	}
	return repository, ok
}

func adminLimit(r *http.Request) int {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		return 50
	}
	if limit > 200 {
		return 200
	}
	return limit
}

func (s *Service) adminOverview(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	result, err := repository.AdminOverview(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) adminRoles(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(authorizationRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "AUTHORIZATION_UNAVAILABLE", "authorization is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	items, err := repository.RoleDefinitions(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) adminRoleBindings(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	userID, err := optionalQueryUUID(r, "user_id")
	if err != nil {
		s.writeError(w, r, ErrConflict)
		return
	}
	organizationID, err := optionalQueryUUID(r, "organization_id")
	if err != nil {
		s.writeError(w, r, ErrConflict)
		return
	}
	items, err := repository.AdminRoleBindings(r.Context(), userID, organizationID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func optionalQueryUUID(r *http.Request, name string) (*uuid.UUID, error) {
	value := strings.TrimSpace(r.URL.Query().Get(name))
	if value == "" {
		return nil, nil
	}
	parsed, err := uuid.Parse(value)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func (s *Service) adminGrantRoleBinding(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	var body roleBindingGrantBody
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	item, err := repository.AdminGrantRoleBinding(r.Context(), mustPrincipal(r.Context()), RoleBindingGrant{UserID: body.UserID, RoleCode: body.RoleCode, OrganizationID: body.OrganizationID, UnitID: body.UnitID, StartsAt: body.StartsAt, ExpiresAt: body.ExpiresAt, IdempotencyKey: body.IdempotencyKey})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusCreated, item)
}

func (s *Service) adminRevokeRoleBinding(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	bindingID, ok := pathUUID(w, r, "binding_id")
	if !ok {
		return
	}
	item, err := repository.AdminRevokeRoleBinding(r.Context(), mustPrincipal(r.Context()), bindingID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, item)
}

func (s *Service) adminUsers(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	items, err := repository.AdminUsers(r.Context(), strings.TrimSpace(r.URL.Query().Get("q")), adminLimit(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) adminUserMemberships(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	userID, ok := pathUUID(w, r, "user_id")
	if !ok {
		return
	}
	items, err := repository.AdminUserMemberships(r.Context(), userID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) adminOrganizations(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	items, err := repository.AdminOrganizations(r.Context(), strings.TrimSpace(r.URL.Query().Get("q")), adminLimit(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) adminCreateOrganization(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	var body struct {
		Name        string    `json:"name"`
		OwnerUserID uuid.UUID `json:"owner_user_id"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || body.OwnerUserID == uuid.Nil {
		if err == nil {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "organization name and owner are required", httpserver.RequestID(r.Context()))
		}
		return
	}
	organization, err := repository.AdminCreateOrganization(r.Context(), mustPrincipal(r.Context()), body.OwnerUserID, body.Name)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusCreated, organization)
}

func (s *Service) adminRenameOrganization(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	if err := repository.AdminRenameOrganization(r.Context(), mustPrincipal(r.Context()), organizationID, body.Name); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) adminUnits(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	items, err := repository.AdminUnits(r.Context(), organizationID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) adminMembers(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	items, err := repository.AdminMembers(r.Context(), organizationID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

type adminMemberRolesBody struct {
	UserID           uuid.UUID   `json:"user_id"`
	Roles            []string    `json:"roles"`
	UnitAdminUnitIDs []uuid.UUID `json:"unit_admin_unit_ids"`
}

func (s *Service) adminAddMember(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	var body adminMemberRolesBody
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	if body.UserID == uuid.Nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "user is required", httpserver.RequestID(r.Context()))
		return
	}
	member, err := repository.AdminAddMember(r.Context(), mustPrincipal(r.Context()), organizationID, body.UserID, body.Roles, body.UnitAdminUnitIDs)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusCreated, member)
}

func (s *Service) adminReplaceMemberRoles(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	userID, ok := pathUUID(w, r, "user_id")
	if !ok {
		return
	}
	var body adminMemberRolesBody
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	member, err := repository.AdminReplaceMemberRoles(r.Context(), mustPrincipal(r.Context()), organizationID, userID, body.Roles, body.UnitAdminUnitIDs)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, member)
}

func (s *Service) adminRemoveMember(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	userID, ok := pathUUID(w, r, "user_id")
	if !ok {
		return
	}
	if err := repository.AdminRemoveMember(r.Context(), mustPrincipal(r.Context()), organizationID, userID); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) adminTransferOwnership(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	var body struct {
		NewOwnerUserID uuid.UUID `json:"new_owner_user_id"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	if body.NewOwnerUserID == uuid.Nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "new owner is required", httpserver.RequestID(r.Context()))
		return
	}
	if err := repository.AdminTransferOwnership(r.Context(), mustPrincipal(r.Context()), organizationID, body.NewOwnerUserID); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) adminCreateUnit(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	var body struct {
		Name     string     `json:"name"`
		ParentID *uuid.UUID `json:"parent_id"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	unit, err := repository.AdminCreateUnit(r.Context(), mustPrincipal(r.Context()), organizationID, body.ParentID, body.Name)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusCreated, unit)
}

func (s *Service) adminRenameUnit(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	unitID, ok := pathUUID(w, r, "unit_id")
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	if err := repository.AdminRenameUnit(r.Context(), mustPrincipal(r.Context()), organizationID, unitID, body.Name); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) adminDeleteUnit(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	unitID, ok := pathUUID(w, r, "unit_id")
	if !ok {
		return
	}
	if err := repository.AdminDeleteUnit(r.Context(), mustPrincipal(r.Context()), organizationID, unitID); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) adminSetUserStatus(w http.ResponseWriter, r *http.Request) {
	s.adminSetStatus(w, r, "user")
}

func (s *Service) adminSetOrganizationStatus(w http.ResponseWriter, r *http.Request) {
	s.adminSetStatus(w, r, "organization")
}

func (s *Service) adminSetStatus(w http.ResponseWriter, r *http.Request, resource string) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	pathName := resource + "_id"
	resourceID, ok := pathUUID(w, r, pathName)
	if !ok {
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	body.Status = strings.ToUpper(strings.TrimSpace(body.Status))
	if body.Status != "ACTIVE" && body.Status != "DISABLED" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "status must be ACTIVE or DISABLED", httpserver.RequestID(r.Context()))
		return
	}
	principal := mustPrincipal(r.Context())
	var err error
	if resource == "user" {
		err = repository.AdminSetUserStatus(r.Context(), principal, resourceID, body.Status)
	} else {
		err = repository.AdminSetOrganizationStatus(r.Context(), principal, resourceID, body.Status)
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) adminDeleteUser(w http.ResponseWriter, r *http.Request) {
	s.adminDelete(w, r, "user")
}

func (s *Service) adminDeleteOrganization(w http.ResponseWriter, r *http.Request) {
	s.adminDelete(w, r, "organization")
}

func (s *Service) adminDelete(w http.ResponseWriter, r *http.Request, resource string) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	resourceID, ok := pathUUID(w, r, resource+"_id")
	if !ok {
		return
	}
	principal := mustPrincipal(r.Context())
	var err error
	if resource == "user" {
		err = repository.AdminDeleteUser(r.Context(), principal, resourceID)
	} else {
		err = repository.AdminDeleteOrganization(r.Context(), principal, resourceID)
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) adminAuditLogs(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	filter, err := parseAdminAuditFilter(r)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "audit filters are invalid", httpserver.RequestID(r.Context()))
		return
	}
	items, err := repository.AdminAuditLogs(r.Context(), filter)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) adminAuditLog(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	auditID, ok := pathUUID(w, r, "audit_id")
	if !ok {
		return
	}
	item, err := repository.AdminAuditLog(r.Context(), auditID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, item)
}

func parseAdminAuditFilter(r *http.Request) (AdminAuditFilter, error) {
	query := r.URL.Query()
	filter := AdminAuditFilter{Actor: strings.TrimSpace(query.Get("actor")), Action: strings.TrimSpace(query.Get("action")), Resource: strings.TrimSpace(query.Get("resource")), Result: strings.ToUpper(strings.TrimSpace(query.Get("result"))), Limit: adminLimit(r)}
	if filter.Result != "" && !slices.Contains([]string{"SUCCEEDED", "DENIED", "FAILED"}, filter.Result) {
		return AdminAuditFilter{}, ErrConflict
	}
	if len(filter.Actor) > 320 || len(filter.Action) > 160 || len(filter.Resource) > 320 {
		return AdminAuditFilter{}, ErrConflict
	}
	for raw, destination := range map[string]**time.Time{"from": &filter.From, "to": &filter.To} {
		if value := strings.TrimSpace(query.Get(raw)); value != "" {
			parsed, err := time.Parse(time.RFC3339, value)
			if err != nil {
				return AdminAuditFilter{}, err
			}
			*destination = &parsed
		}
	}
	if filter.From != nil && filter.To != nil && filter.From.After(*filter.To) {
		return AdminAuditFilter{}, ErrConflict
	}
	return filter, nil
}

func (r *PostgresRepository) AdminOverview(ctx context.Context) (AdminOverview, error) {
	var result AdminOverview
	err := r.pool.QueryRow(ctx, `
SELECT
  (SELECT count(*) FROM users WHERE status<>'DELETED'),
  (SELECT count(*) FROM users WHERE status='ACTIVE'),
  (SELECT count(*) FROM users WHERE status='DISABLED'),
  (SELECT count(*) FROM organizations WHERE status<>'DELETED'),
  (SELECT count(*) FROM memberships WHERE status='ACTIVE'),
  (SELECT count(*) FROM invitations WHERE status='PENDING' AND expires_at > now())`).Scan(
		&result.Users, &result.ActiveUsers, &result.DisabledUsers, &result.Organizations,
		&result.ActiveMemberships, &result.PendingInvitations,
	)
	return result, err
}

func (r *PostgresRepository) AdminUsers(ctx context.Context, search string, limit int) ([]AdminUser, error) {
	pattern := "%" + strings.ToLower(search) + "%"
	rows, err := r.pool.Query(ctx, `
SELECT u.id,u.user_code,u.username,u.display_name,u.email,u.status,
       (SELECT count(*) FROM memberships m WHERE m.user_id=u.id AND m.status='ACTIVE'),
       (SELECT max(s.last_seen_at) FROM user_sessions s WHERE s.user_id=u.id),
       u.created_at,u.updated_at
FROM users u
WHERE u.status<>'DELETED' AND ($1='' OR lower(COALESCE(u.username,'')) LIKE $2 OR lower(COALESCE(u.display_name,'')) LIKE $2 OR lower(COALESCE(u.email,'')) LIKE $2 OR u.id::text=$1)
ORDER BY u.updated_at DESC,u.id LIMIT $3`, strings.ToLower(search), pattern, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AdminUser, 0)
	for rows.Next() {
		var item AdminUser
		if err := rows.Scan(&item.ID, &item.UserCode, &item.Username, &item.DisplayName, &item.Email, &item.Status, &item.OrganizationCount, &item.LastSeenAt, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		item.UserCode = FormatUserCode(item.UserCode)
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminUserMemberships(ctx context.Context, userID uuid.UUID) ([]AdminUserMembership, error) {
	var exists bool
	if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND status<>'DELETED')`, userID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := r.pool.Query(ctx, `
SELECT o.id,o.name,m.status,
       ARRAY(SELECT DISTINCT rb.role FROM role_bindings rb WHERE rb.membership_id=m.id AND rb.revoked_at IS NULL AND rb.starts_at<=now() AND (rb.expires_at IS NULL OR rb.expires_at>now()) ORDER BY rb.role),
       ARRAY(SELECT rb.unit_id FROM role_bindings rb WHERE rb.membership_id=m.id AND rb.role='UNIT_ADMIN' AND rb.revoked_at IS NULL AND rb.starts_at<=now() AND (rb.expires_at IS NULL OR rb.expires_at>now()) ORDER BY rb.unit_id)
FROM memberships m JOIN organizations o ON o.id=m.organization_id
WHERE m.user_id=$1 AND m.status<>'REVOKED' AND o.status<>'DELETED'
ORDER BY lower(o.name),o.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AdminUserMembership, 0)
	for rows.Next() {
		var item AdminUserMembership
		if err := rows.Scan(&item.OrganizationID, &item.OrganizationName, &item.MembershipStatus, &item.Roles, &item.UnitAdminIDs); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminOrganizations(ctx context.Context, search string, limit int) ([]AdminOrganization, error) {
	pattern := "%" + strings.ToLower(search) + "%"
	rows, err := r.pool.Query(ctx, `
SELECT o.id,o.name,o.status,count(m.id) FILTER (WHERE m.status='ACTIVE'),o.created_at,o.updated_at
FROM organizations o LEFT JOIN memberships m ON m.organization_id=o.id
WHERE o.status<>'DELETED' AND ($1='' OR lower(o.name) LIKE $2 OR o.id::text=$1)
GROUP BY o.id ORDER BY o.updated_at DESC,o.id LIMIT $3`, strings.ToLower(search), pattern, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AdminOrganization, 0)
	for rows.Next() {
		var item AdminOrganization
		if err := rows.Scan(&item.ID, &item.Name, &item.Status, &item.MemberCount, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminCreateOrganization(ctx context.Context, principal identity.Principal, ownerUserID uuid.UUID, name string) (Organization, error) {
	name = strings.TrimSpace(name)
	if ownerUserID == uuid.Nil || name == "" || len([]rune(name)) > 120 {
		return Organization{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Organization{}, err
	}
	defer tx.Rollback(ctx)
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return Organization{}, err
	}
	var ownerStatus string
	err = tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1 FOR UPDATE`, ownerUserID).Scan(&ownerStatus)
	if errors.Is(err, pgx.ErrNoRows) || ownerStatus == "DELETED" {
		return Organization{}, ErrNotFound
	}
	if err != nil {
		return Organization{}, err
	}
	if ownerStatus != "ACTIVE" {
		return Organization{}, ErrConflict
	}
	now := r.now()
	organization := Organization{ID: uuid.Must(uuid.NewV7()), Name: name, Status: "ACTIVE", Version: 1, Roles: []string{"OWNER"}}
	membershipID := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO organizations(id,name,status,created_by,created_at,updated_at) VALUES($1,$2,'ACTIVE',$3,$4,$4)`, organization.ID, name, ownerUserID, now); err != nil {
		return Organization{}, mapConstraintError(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO memberships(id,organization_id,user_id,status,joined_at,updated_at) VALUES($1,$2,$3,'ACTIVE',$4,$4)`, membershipID, organization.ID, ownerUserID, now); err != nil {
		return Organization{}, mapConstraintError(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO role_bindings(id,membership_id,role,created_by,created_at,grant_source) VALUES($1,$2,'OWNER',$3,$4,'ORGANIZATION_CREATION')`, uuid.Must(uuid.NewV7()), membershipID, ownerUserID, now); err != nil {
		return Organization{}, mapConstraintError(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET membership_version=membership_version+1,updated_at=$2 WHERE id=$1`, ownerUserID, now); err != nil {
		return Organization{}, err
	}
	if err := appendAdminAudit(ctx, tx, actorID, principal, &organization.ID, "admin.organization.create", "organization", organization.ID.String(), map[string]any{"name": name, "owner_user_id": ownerUserID}); err != nil {
		return Organization{}, err
	}
	if err := appendOutbox(ctx, tx, "account.organization.created.v1", organization.ID, 1, map[string]any{"organization_id": organization.ID, "status": "ACTIVE"}); err != nil {
		return Organization{}, err
	}
	return organization, tx.Commit(ctx)
}

func (r *PostgresRepository) AdminRenameOrganization(ctx context.Context, principal identity.Principal, organizationID uuid.UUID, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 120 {
		return ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return err
	}
	var currentName, status string
	err = tx.QueryRow(ctx, `SELECT name,status FROM organizations WHERE id=$1 FOR UPDATE`, organizationID).Scan(&currentName, &status)
	if errors.Is(err, pgx.ErrNoRows) || status == "DELETED" {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if currentName == name {
		return tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `UPDATE organizations SET name=$2,version=version+1,updated_at=$3 WHERE id=$1`, organizationID, name, r.now()); err != nil {
		return mapConstraintError(err)
	}
	if err := appendAdminAudit(ctx, tx, actorID, principal, &organizationID, "admin.organization.rename", "organization", organizationID.String(), map[string]any{"from": currentName, "to": name}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) AdminUnits(ctx context.Context, organizationID uuid.UUID) ([]Unit, error) {
	var exists bool
	if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organizations WHERE id=$1 AND status<>'DELETED')`, organizationID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := r.pool.Query(ctx, `SELECT id,organization_id,parent_id,name,status,version FROM organization_units WHERE organization_id=$1 ORDER BY created_at,id`, organizationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Unit, 0)
	for rows.Next() {
		var item Unit
		if err := rows.Scan(&item.ID, &item.OrganizationID, &item.ParentID, &item.Name, &item.Status, &item.Version); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminMembers(ctx context.Context, organizationID uuid.UUID) ([]Member, error) {
	var exists bool
	if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organizations WHERE id=$1 AND status<>'DELETED')`, organizationID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := r.pool.Query(ctx, `
SELECT m.id,m.user_id,u.display_name,u.email,m.status,m.version,m.joined_at,
       ARRAY(SELECT DISTINCT rb.role FROM role_bindings rb WHERE rb.membership_id=m.id AND rb.revoked_at IS NULL AND rb.starts_at<=now() AND (rb.expires_at IS NULL OR rb.expires_at>now()) ORDER BY rb.role),
       ARRAY(SELECT mu.unit_id FROM membership_units mu WHERE mu.membership_id=m.id ORDER BY mu.unit_id),
       ARRAY(SELECT rb.unit_id FROM role_bindings rb WHERE rb.membership_id=m.id AND rb.role='UNIT_ADMIN' AND rb.revoked_at IS NULL AND rb.starts_at<=now() AND (rb.expires_at IS NULL OR rb.expires_at>now()) ORDER BY rb.unit_id)
FROM memberships m JOIN users u ON u.id=m.user_id
WHERE m.organization_id=$1 AND m.status<>'REVOKED' AND u.status='ACTIVE'
ORDER BY CASE WHEN EXISTS(SELECT 1 FROM role_bindings rb WHERE rb.membership_id=m.id AND rb.role='OWNER' AND rb.revoked_at IS NULL AND rb.starts_at<=now() AND (rb.expires_at IS NULL OR rb.expires_at>now())) THEN 0 ELSE 1 END,
         lower(COALESCE(u.display_name,u.email,'')),m.user_id`, organizationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Member, 0)
	for rows.Next() {
		var item Member
		if err := rows.Scan(&item.MembershipID, &item.UserID, &item.DisplayName, &item.Email, &item.Status, &item.Version, &item.JoinedAt, &item.Roles, &item.UnitIDs, &item.UnitAdminIDs); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func normalizeAdminMemberRoles(roles []string, unitAdminIDs []uuid.UUID) ([]string, []uuid.UUID, error) {
	allowed := map[string]bool{"ORG_ADMIN": true, "BILLING_ADMIN": true, "AUDITOR": true, "MEMBER": true}
	roleSet := map[string]bool{"MEMBER": true}
	for _, role := range roles {
		role = strings.ToUpper(strings.TrimSpace(role))
		if !allowed[role] {
			return nil, nil, ErrConflict
		}
		roleSet[role] = true
	}
	normalizedRoles := make([]string, 0, len(roleSet))
	for role := range roleSet {
		normalizedRoles = append(normalizedRoles, role)
	}
	sort.Strings(normalizedRoles)
	unitSet := map[uuid.UUID]bool{}
	normalizedUnits := make([]uuid.UUID, 0, len(unitAdminIDs))
	for _, unitID := range unitAdminIDs {
		if unitID == uuid.Nil || unitSet[unitID] {
			if unitID == uuid.Nil {
				return nil, nil, ErrConflict
			}
			continue
		}
		unitSet[unitID] = true
		normalizedUnits = append(normalizedUnits, unitID)
	}
	sort.Slice(normalizedUnits, func(i, j int) bool { return normalizedUnits[i].String() < normalizedUnits[j].String() })
	return normalizedRoles, normalizedUnits, nil
}

func validateAdminUnits(ctx context.Context, tx pgx.Tx, organizationID uuid.UUID, unitIDs []uuid.UUID) error {
	for _, unitID := range unitIDs {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organization_units WHERE id=$1 AND organization_id=$2 AND status='ACTIVE')`, unitID, organizationID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
	}
	return nil
}

func replaceAdminMemberRoles(ctx context.Context, tx pgx.Tx, membershipID, userID, actorID uuid.UUID, roles []string, unitAdminIDs []uuid.UUID, now time.Time) (int64, error) {
	if _, err := tx.Exec(ctx, `UPDATE role_bindings SET revoked_at=$2,revoked_by=$3 WHERE membership_id=$1 AND role<>'OWNER' AND revoked_at IS NULL`, membershipID, now, actorID); err != nil {
		return 0, err
	}
	for _, role := range roles {
		if _, err := tx.Exec(ctx, `INSERT INTO role_bindings(id,membership_id,role,created_by,created_at,grant_source) VALUES($1,$2,$3,$4,$5,'ADMIN')`, uuid.Must(uuid.NewV7()), membershipID, role, actorID, now); err != nil {
			return 0, mapConstraintError(err)
		}
	}
	for _, unitID := range unitAdminIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO role_bindings(id,membership_id,role,unit_id,created_by,created_at,grant_source) VALUES($1,$2,'UNIT_ADMIN',$3,$4,$5,'ADMIN')`, uuid.Must(uuid.NewV7()), membershipID, unitID, actorID, now); err != nil {
			return 0, mapConstraintError(err)
		}
	}
	var version int64
	if err := tx.QueryRow(ctx, `UPDATE memberships SET version=version+1,updated_at=$2 WHERE id=$1 RETURNING version`, membershipID, now).Scan(&version); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET membership_version=membership_version+1,updated_at=$2 WHERE id=$1`, userID, now); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE user_sessions SET membership_version=membership_version+1,last_seen_at=$2 WHERE user_id=$1`, userID, now); err != nil {
		return 0, err
	}
	return version, nil
}

func (r *PostgresRepository) AdminAddMember(ctx context.Context, principal identity.Principal, organizationID, userID uuid.UUID, roles []string, unitAdminIDs []uuid.UUID) (Member, error) {
	roles, unitAdminIDs, err := normalizeAdminMemberRoles(roles, unitAdminIDs)
	if err != nil {
		return Member{}, err
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Member{}, err
	}
	defer tx.Rollback(ctx)
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return Member{}, err
	}
	var organizationStatus, userStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM organizations WHERE id=$1 FOR UPDATE`, organizationID).Scan(&organizationStatus); errors.Is(err, pgx.ErrNoRows) || organizationStatus == "DELETED" {
		return Member{}, ErrNotFound
	} else if err != nil {
		return Member{}, err
	}
	if err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&userStatus); errors.Is(err, pgx.ErrNoRows) || userStatus == "DELETED" {
		return Member{}, ErrNotFound
	} else if err != nil {
		return Member{}, err
	}
	if organizationStatus != "ACTIVE" || userStatus != "ACTIVE" {
		return Member{}, ErrConflict
	}
	if err := validateAdminUnits(ctx, tx, organizationID, unitAdminIDs); err != nil {
		return Member{}, err
	}
	var membershipID uuid.UUID
	var status string
	err = tx.QueryRow(ctx, `SELECT id,status FROM memberships WHERE organization_id=$1 AND user_id=$2 FOR UPDATE`, organizationID, userID).Scan(&membershipID, &status)
	now := r.now()
	if errors.Is(err, pgx.ErrNoRows) {
		membershipID = uuid.Must(uuid.NewV7())
		if _, err := tx.Exec(ctx, `INSERT INTO memberships(id,organization_id,user_id,status,joined_at,updated_at) VALUES($1,$2,$3,'ACTIVE',$4,$4)`, membershipID, organizationID, userID, now); err != nil {
			return Member{}, mapConstraintError(err)
		}
	} else if err != nil {
		return Member{}, err
	} else if status == "ACTIVE" {
		return Member{}, ErrConflict
	} else if _, err := tx.Exec(ctx, `UPDATE memberships SET status='ACTIVE',version=version+1,updated_at=$2 WHERE id=$1`, membershipID, now); err != nil {
		return Member{}, err
	}
	createdBy := userID
	if actorID != nil {
		createdBy = *actorID
	}
	version, err := replaceAdminMemberRoles(ctx, tx, membershipID, userID, createdBy, roles, unitAdminIDs, now)
	if err != nil {
		return Member{}, err
	}
	if err := appendAdminAudit(ctx, tx, actorID, principal, &organizationID, "admin.membership.add", "membership", membershipID.String(), map[string]any{"user_id": userID, "roles": roles, "unit_admin_unit_ids": unitAdminIDs}); err != nil {
		return Member{}, err
	}
	if err := appendOutbox(ctx, tx, "account.membership.changed.v1", membershipID, version, map[string]any{"organization_id": organizationID, "user_id": userID, "membership_status": "ACTIVE", "membership_version": version}); err != nil {
		return Member{}, err
	}
	member, err := getMember(ctx, tx, membershipID)
	if err != nil {
		return Member{}, err
	}
	return member, tx.Commit(ctx)
}

func (r *PostgresRepository) AdminReplaceMemberRoles(ctx context.Context, principal identity.Principal, organizationID, userID uuid.UUID, roles []string, unitAdminIDs []uuid.UUID) (Member, error) {
	roles, unitAdminIDs, err := normalizeAdminMemberRoles(roles, unitAdminIDs)
	if err != nil {
		return Member{}, err
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Member{}, err
	}
	defer tx.Rollback(ctx)
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return Member{}, err
	}
	if err := validateAdminUnits(ctx, tx, organizationID, unitAdminIDs); err != nil {
		return Member{}, err
	}
	var membershipID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM memberships WHERE organization_id=$1 AND user_id=$2 AND status='ACTIVE' FOR UPDATE`, organizationID, userID).Scan(&membershipID); errors.Is(err, pgx.ErrNoRows) {
		return Member{}, ErrNotFound
	} else if err != nil {
		return Member{}, err
	}
	now := r.now()
	createdBy := userID
	if actorID != nil {
		createdBy = *actorID
	}
	version, err := replaceAdminMemberRoles(ctx, tx, membershipID, userID, createdBy, roles, unitAdminIDs, now)
	if err != nil {
		return Member{}, err
	}
	if err := appendAdminAudit(ctx, tx, actorID, principal, &organizationID, "admin.membership.roles.replace", "membership", membershipID.String(), map[string]any{"user_id": userID, "roles": roles, "unit_admin_unit_ids": unitAdminIDs}); err != nil {
		return Member{}, err
	}
	if err := appendOutbox(ctx, tx, "account.membership.changed.v1", membershipID, version, map[string]any{"organization_id": organizationID, "user_id": userID, "membership_status": "ACTIVE", "membership_version": version}); err != nil {
		return Member{}, err
	}
	member, err := getMember(ctx, tx, membershipID)
	if err != nil {
		return Member{}, err
	}
	return member, tx.Commit(ctx)
}

func (r *PostgresRepository) AdminRemoveMember(ctx context.Context, principal identity.Principal, organizationID, userID uuid.UUID) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return err
	}
	var membershipID uuid.UUID
	var version int64
	var owner bool
	err = tx.QueryRow(ctx, `SELECT m.id,m.version,EXISTS(SELECT 1 FROM role_bindings rb WHERE rb.membership_id=m.id AND rb.role='OWNER') FROM memberships m WHERE m.organization_id=$1 AND m.user_id=$2 AND m.status='ACTIVE' FOR UPDATE`, organizationID, userID).Scan(&membershipID, &version, &owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if owner {
		return ErrLastOwner
	}
	now := r.now()
	version++
	if _, err := tx.Exec(ctx, `DELETE FROM role_bindings WHERE membership_id=$1`, membershipID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM membership_units WHERE membership_id=$1`, membershipID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE memberships SET status='REVOKED',version=$2,updated_at=$3 WHERE id=$1`, membershipID, version, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET membership_version=membership_version+1,updated_at=$2 WHERE id=$1`, userID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE user_sessions SET membership_version=membership_version+1,last_seen_at=$2 WHERE user_id=$1`, userID, now); err != nil {
		return err
	}
	if err := appendAdminAudit(ctx, tx, actorID, principal, &organizationID, "admin.membership.remove", "membership", membershipID.String(), map[string]any{"user_id": userID}); err != nil {
		return err
	}
	if err := appendOutbox(ctx, tx, "account.membership.changed.v1", membershipID, version, map[string]any{"organization_id": organizationID, "user_id": userID, "membership_status": "REVOKED", "membership_version": version}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) AdminTransferOwnership(ctx context.Context, principal identity.Principal, organizationID, targetUserID uuid.UUID) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, organizationID.String()); err != nil {
		return err
	}
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return err
	}
	var organizationStatus string
	err = tx.QueryRow(ctx, `SELECT status FROM organizations WHERE id=$1 FOR UPDATE`, organizationID).Scan(&organizationStatus)
	if errors.Is(err, pgx.ErrNoRows) || organizationStatus == "DELETED" {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if organizationStatus != "ACTIVE" {
		return ErrConflict
	}
	var targetMembership uuid.UUID
	err = tx.QueryRow(ctx, `
SELECT m.id FROM memberships m JOIN users u ON u.id=m.user_id
WHERE m.organization_id=$1 AND m.user_id=$2 AND m.status='ACTIVE' AND u.status='ACTIVE'
FOR UPDATE OF m,u`, organizationID, targetUserID).Scan(&targetMembership)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `
SELECT m.id,m.user_id FROM memberships m JOIN role_bindings rb ON rb.membership_id=m.id
WHERE m.organization_id=$1 AND m.status='ACTIVE' AND rb.role='OWNER'
FOR UPDATE OF m,rb`, organizationID)
	if err != nil {
		return err
	}
	ownerMemberships := make([]uuid.UUID, 0)
	previousOwners := make([]uuid.UUID, 0)
	for rows.Next() {
		var membershipID, userID uuid.UUID
		if err := rows.Scan(&membershipID, &userID); err != nil {
			rows.Close()
			return err
		}
		ownerMemberships = append(ownerMemberships, membershipID)
		previousOwners = append(previousOwners, userID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(previousOwners) == 1 && previousOwners[0] == targetUserID {
		return tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM role_bindings rb USING memberships m WHERE rb.membership_id=m.id AND m.organization_id=$1 AND rb.role='OWNER'`, organizationID); err != nil {
		return err
	}
	createdBy := targetUserID
	if actorID != nil {
		createdBy = *actorID
	}
	now := r.now()
	if _, err := tx.Exec(ctx, `INSERT INTO role_bindings(id,membership_id,role,created_by,created_at,grant_source) VALUES($1,$2,'OWNER',$3,$4,'OWNERSHIP_TRANSFER')`, uuid.Must(uuid.NewV7()), targetMembership, createdBy, now); err != nil {
		return mapConstraintError(err)
	}
	affectedMemberships := append(ownerMemberships, targetMembership)
	if _, err := tx.Exec(ctx, `UPDATE memberships SET version=version+1,updated_at=$2 WHERE id=ANY($1)`, affectedMemberships, now); err != nil {
		return err
	}
	affectedUsers := append(previousOwners, targetUserID)
	if _, err := tx.Exec(ctx, `UPDATE users SET membership_version=membership_version+1,updated_at=$2 WHERE id=ANY($1)`, affectedUsers, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE user_sessions SET membership_version=membership_version+1,last_seen_at=$2 WHERE user_id=ANY($1)`, affectedUsers, now); err != nil {
		return err
	}
	if err := appendAdminAudit(ctx, tx, actorID, principal, &organizationID, "admin.organization.ownership.transfer", "organization", organizationID.String(), map[string]any{"previous_owner_user_ids": previousOwners, "new_owner_user_id": targetUserID}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) AdminCreateUnit(ctx context.Context, principal identity.Principal, organizationID uuid.UUID, parentID *uuid.UUID, name string) (Unit, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 120 {
		return Unit{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Unit{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, organizationID.String()); err != nil {
		return Unit{}, err
	}
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return Unit{}, err
	}
	var organizationStatus string
	err = tx.QueryRow(ctx, `SELECT status FROM organizations WHERE id=$1 FOR UPDATE`, organizationID).Scan(&organizationStatus)
	if errors.Is(err, pgx.ErrNoRows) || organizationStatus == "DELETED" {
		return Unit{}, ErrNotFound
	}
	if err != nil {
		return Unit{}, err
	}
	if organizationStatus != "ACTIVE" {
		return Unit{}, ErrConflict
	}
	if parentID != nil {
		var parentActive bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organization_units WHERE organization_id=$1 AND id=$2 AND status='ACTIVE')`, organizationID, *parentID).Scan(&parentActive); err != nil {
			return Unit{}, err
		}
		if !parentActive {
			return Unit{}, ErrInvalidHierarchy
		}
	}
	now := r.now()
	unit := Unit{ID: uuid.Must(uuid.NewV7()), OrganizationID: organizationID, ParentID: parentID, Name: name, Status: "ACTIVE", Version: 1}
	if _, err := tx.Exec(ctx, `INSERT INTO organization_units(id,organization_id,parent_id,name,status,created_at,updated_at) VALUES($1,$2,$3,$4,'ACTIVE',$5,$5)`, unit.ID, organizationID, parentID, name, now); err != nil {
		return Unit{}, mapConstraintError(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO organization_unit_closure(organization_id,ancestor_id,descendant_id,depth) VALUES($1,$2,$2,0)`, organizationID, unit.ID); err != nil {
		return Unit{}, err
	}
	if parentID != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO organization_unit_closure(organization_id,ancestor_id,descendant_id,depth) SELECT organization_id,ancestor_id,$3,depth+1 FROM organization_unit_closure WHERE organization_id=$1 AND descendant_id=$2`, organizationID, *parentID, unit.ID); err != nil {
			return Unit{}, err
		}
	}
	if err := appendAdminAudit(ctx, tx, actorID, principal, &organizationID, "admin.organization_unit.create", "organization_unit", unit.ID.String(), map[string]any{"name": name, "parent_id": parentID}); err != nil {
		return Unit{}, err
	}
	return unit, tx.Commit(ctx)
}

func (r *PostgresRepository) AdminRenameUnit(ctx context.Context, principal identity.Principal, organizationID, unitID uuid.UUID, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 120 {
		return ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, organizationID.String()); err != nil {
		return err
	}
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return err
	}
	var organizationStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM organizations WHERE id=$1 FOR UPDATE`, organizationID).Scan(&organizationStatus); errors.Is(err, pgx.ErrNoRows) || organizationStatus == "DELETED" {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	var currentName, unitStatus string
	err = tx.QueryRow(ctx, `SELECT name,status FROM organization_units WHERE organization_id=$1 AND id=$2 FOR UPDATE`, organizationID, unitID).Scan(&currentName, &unitStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if unitStatus != "ACTIVE" {
		return ErrConflict
	}
	if currentName == name {
		return tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `UPDATE organization_units SET name=$3,version=version+1,updated_at=$4 WHERE organization_id=$1 AND id=$2`, organizationID, unitID, name, r.now()); err != nil {
		return mapConstraintError(err)
	}
	if err := appendAdminAudit(ctx, tx, actorID, principal, &organizationID, "admin.organization_unit.rename", "organization_unit", unitID.String(), map[string]any{"from": currentName, "to": name}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) AdminDeleteUnit(ctx context.Context, principal identity.Principal, organizationID, unitID uuid.UUID) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, organizationID.String()); err != nil {
		return err
	}
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return err
	}
	var organizationStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM organizations WHERE id=$1 FOR UPDATE`, organizationID).Scan(&organizationStatus); errors.Is(err, pgx.ErrNoRows) || organizationStatus == "DELETED" {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	var name, unitStatus string
	err = tx.QueryRow(ctx, `SELECT name,status FROM organization_units WHERE organization_id=$1 AND id=$2 FOR UPDATE`, organizationID, unitID).Scan(&name, &unitStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if unitStatus != "ACTIVE" {
		return ErrConflict
	}
	var hasChildren bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organization_units WHERE organization_id=$1 AND parent_id=$2)`, organizationID, unitID).Scan(&hasChildren); err != nil {
		return err
	}
	if hasChildren {
		return ErrInvalidHierarchy
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT m.user_id FROM memberships m LEFT JOIN membership_units mu ON mu.membership_id=m.id LEFT JOIN role_bindings rb ON rb.membership_id=m.id WHERE m.organization_id=$1 AND (mu.unit_id=$2 OR rb.unit_id=$2)`, organizationID, unitID)
	if err != nil {
		return err
	}
	affectedUsers := make([]uuid.UUID, 0)
	for rows.Next() {
		var userID uuid.UUID
		if err := rows.Scan(&userID); err != nil {
			rows.Close()
			return err
		}
		affectedUsers = append(affectedUsers, userID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if _, err := tx.Exec(ctx, `DELETE FROM role_bindings WHERE unit_id=$1`, unitID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM membership_units WHERE unit_id=$1`, unitID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM organization_units WHERE organization_id=$1 AND id=$2`, organizationID, unitID); err != nil {
		return mapConstraintError(err)
	}
	if len(affectedUsers) > 0 {
		now := r.now()
		if _, err := tx.Exec(ctx, `UPDATE users SET membership_version=membership_version+1,updated_at=$2 WHERE id=ANY($1)`, affectedUsers, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE user_sessions SET membership_version=membership_version+1,last_seen_at=$2 WHERE user_id=ANY($1)`, affectedUsers, now); err != nil {
			return err
		}
	}
	if err := appendAdminAudit(ctx, tx, actorID, principal, &organizationID, "admin.organization_unit.delete", "organization_unit", unitID.String(), map[string]any{"name": name}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const adminAuditSelect = `SELECT log.id,log.actor_user_id,actor.display_name,actor.email,log.active_organization_id,log.action,log.resource_type,log.resource_id,log.result,log.request_id,log.change_summary,log.occurred_at FROM audit_logs log LEFT JOIN users actor ON actor.id=log.actor_user_id`

func (r *PostgresRepository) AdminAuditLogs(ctx context.Context, filter AdminAuditFilter) ([]AdminAuditLog, error) {
	rows, err := r.pool.Query(ctx, adminAuditSelect+`
WHERE ($1='' OR COALESCE(log.actor_user_id::text,'') ILIKE '%'||$1||'%' OR COALESCE(actor.display_name,'') ILIKE '%'||$1||'%' OR COALESCE(actor.email,'') ILIKE '%'||$1||'%' OR COALESCE(log.change_summary->>'actor_subject','') ILIKE '%'||$1||'%')
  AND ($2='' OR log.action ILIKE '%'||$2||'%')
  AND ($3='' OR log.resource_type ILIKE '%'||$3||'%' OR log.resource_id ILIKE '%'||$3||'%')
  AND ($4='' OR log.result=$4)
  AND ($5::timestamptz IS NULL OR log.occurred_at >= $5)
  AND ($6::timestamptz IS NULL OR log.occurred_at <= $6)
ORDER BY log.occurred_at DESC,log.id LIMIT $7`, filter.Actor, filter.Action, filter.Resource, filter.Result, filter.From, filter.To, filter.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AdminAuditLog, 0)
	for rows.Next() {
		var item AdminAuditLog
		if err := rows.Scan(&item.ID, &item.ActorUserID, &item.ActorDisplayName, &item.ActorEmail, &item.ActiveOrganizationID, &item.Action, &item.ResourceType, &item.ResourceID, &item.Result, &item.RequestID, &item.ChangeSummary, &item.OccurredAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminAuditLog(ctx context.Context, auditID uuid.UUID) (AdminAuditLog, error) {
	var item AdminAuditLog
	err := r.pool.QueryRow(ctx, adminAuditSelect+` WHERE log.id=$1`, auditID).Scan(&item.ID, &item.ActorUserID, &item.ActorDisplayName, &item.ActorEmail, &item.ActiveOrganizationID, &item.Action, &item.ResourceType, &item.ResourceID, &item.Result, &item.RequestID, &item.ChangeSummary, &item.OccurredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminAuditLog{}, ErrNotFound
	}
	return item, err
}

func (r *PostgresRepository) AdminSetUserStatus(ctx context.Context, principal identity.Principal, userID uuid.UUID, status string) error {
	if status != "ACTIVE" && status != "DISABLED" {
		return ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return err
	}
	if actorID != nil && *actorID == userID {
		return ErrConflict
	}
	var currentStatus string
	err = tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&currentStatus)
	if errors.Is(err, pgx.ErrNoRows) || currentStatus == "DELETED" {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if currentStatus == status {
		return tx.Commit(ctx)
	}
	if (status == "ACTIVE" && currentStatus != "DISABLED") || (status == "DISABLED" && currentStatus != "ACTIVE" && currentStatus != "PROVISIONING") {
		return ErrConflict
	}
	if status == "DISABLED" {
		if err := assertNotLastActiveSuperAdmin(ctx, tx, userID); err != nil {
			return err
		}
	}
	now := r.now()
	if _, err := tx.Exec(ctx, `UPDATE users SET status=$2,version=version+1,membership_version=membership_version+1,updated_at=$3 WHERE id=$1`, userID, status, now); err != nil {
		return err
	}
	if status == "DISABLED" {
		if _, err := tx.Exec(ctx, `DELETE FROM user_sessions WHERE user_id=$1`, userID); err != nil {
			return err
		}
	}
	if err := appendAdminAudit(ctx, tx, actorID, principal, nil, "admin.user.status.update", "user", userID.String(), map[string]any{"from": currentStatus, "to": status}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) AdminDeleteUser(ctx context.Context, principal identity.Principal, userID uuid.UUID) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return err
	}
	if actorID != nil && *actorID == userID {
		return ErrConflict
	}
	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) || status == "DELETED" {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var ownsOrganization bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS(
  SELECT 1 FROM memberships m
  JOIN role_bindings rb ON rb.membership_id=m.id AND rb.role='OWNER'
  JOIN organizations o ON o.id=m.organization_id AND o.status<>'DELETED'
  WHERE m.user_id=$1 AND m.status IN ('ACTIVE','SUSPENDED')
)`, userID).Scan(&ownsOrganization); err != nil {
		return err
	}
	if ownsOrganization {
		return ErrLastOwner
	}
	if err := assertNotLastActiveSuperAdmin(ctx, tx, userID); err != nil {
		return err
	}
	now := r.now()
	if _, err := tx.Exec(ctx, `UPDATE memberships SET status='REVOKED',version=version+1,updated_at=$2 WHERE user_id=$1 AND status<>'REVOKED'`, userID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_sessions WHERE user_id=$1`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE invitations SET email_normalized='deleted+'||left(id::text,8)||'@invalid.local',updated_at=$2 WHERE accepted_by=$1`, userID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET status='DELETED',display_name=NULL,email=NULL,version=version+1,membership_version=membership_version+1,updated_at=$2 WHERE id=$1`, userID, now); err != nil {
		return err
	}
	if err := appendAdminAudit(ctx, tx, actorID, principal, nil, "admin.user.delete", "user", userID.String(), map[string]any{"from": status, "to": "DELETED"}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) AdminSetOrganizationStatus(ctx context.Context, principal identity.Principal, organizationID uuid.UUID, status string) error {
	if status != "ACTIVE" && status != "DISABLED" {
		return ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return err
	}
	var currentStatus string
	err = tx.QueryRow(ctx, `SELECT status FROM organizations WHERE id=$1 FOR UPDATE`, organizationID).Scan(&currentStatus)
	if errors.Is(err, pgx.ErrNoRows) || currentStatus == "DELETED" {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if currentStatus == status {
		return tx.Commit(ctx)
	}
	if (status == "ACTIVE" && currentStatus != "DISABLED") || (status == "DISABLED" && currentStatus != "ACTIVE") {
		return ErrConflict
	}
	now := r.now()
	if _, err := tx.Exec(ctx, `UPDATE organizations SET status=$2,version=version+1,updated_at=$3 WHERE id=$1`, organizationID, status, now); err != nil {
		return err
	}
	fromMembershipStatus, toMembershipStatus := "SUSPENDED", "ACTIVE"
	if status == "DISABLED" {
		fromMembershipStatus, toMembershipStatus = "ACTIVE", "SUSPENDED"
	}
	if _, err := tx.Exec(ctx, `
WITH changed AS (
  UPDATE memberships SET status=$2,version=version+1,updated_at=$4
  WHERE organization_id=$1 AND status=$3 RETURNING user_id
)
UPDATE users SET membership_version=membership_version+1,updated_at=$4
WHERE id IN (SELECT user_id FROM changed)`, organizationID, toMembershipStatus, fromMembershipStatus, now); err != nil {
		return err
	}
	if status == "DISABLED" {
		if _, err := tx.Exec(ctx, `UPDATE user_sessions SET active_context_type='PERSONAL',active_organization_id=NULL,membership_version=membership_version+1,last_seen_at=$2 WHERE active_organization_id=$1`, organizationID, now); err != nil {
			return err
		}
	}
	if err := appendAdminAudit(ctx, tx, actorID, principal, &organizationID, "admin.organization.status.update", "organization", organizationID.String(), map[string]any{"from": currentStatus, "to": status}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) AdminDeleteOrganization(ctx context.Context, principal identity.Principal, organizationID uuid.UUID) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return err
	}
	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM organizations WHERE id=$1 FOR UPDATE`, organizationID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) || status == "DELETED" {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	now := r.now()
	if _, err := tx.Exec(ctx, `
WITH changed AS (
  UPDATE memberships SET status='REVOKED',version=version+1,updated_at=$2
  WHERE organization_id=$1 AND status<>'REVOKED' RETURNING user_id
)
UPDATE users SET membership_version=membership_version+1,updated_at=$2
WHERE id IN (SELECT user_id FROM changed)`, organizationID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE user_sessions SET active_context_type='PERSONAL',active_organization_id=NULL,membership_version=membership_version+1,last_seen_at=$2 WHERE active_organization_id=$1`, organizationID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE organization_units SET status='DISABLED',name='Deleted unit '||left(id::text,8),version=version+1,updated_at=$2 WHERE organization_id=$1`, organizationID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE invitations SET status=CASE WHEN status='PENDING' THEN 'REVOKED' ELSE status END,email_normalized='deleted+'||left(id::text,8)||'@invalid.local',updated_at=$2 WHERE organization_id=$1`, organizationID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE organizations SET status='DELETED',name='Deleted organization '||left(id::text,8),version=version+1,updated_at=$2 WHERE id=$1`, organizationID, now); err != nil {
		return err
	}
	if err := appendAdminAudit(ctx, tx, actorID, principal, &organizationID, "admin.organization.delete", "organization", organizationID.String(), map[string]any{"from": status, "to": "DELETED"}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func adminActorID(ctx context.Context, tx pgx.Tx, principal identity.Principal) (*uuid.UUID, error) {
	var actorID uuid.UUID
	err := tx.QueryRow(ctx, `SELECT user_id FROM identities WHERE issuer=$1 AND subject=$2`, principal.Issuer, principal.Subject).Scan(&actorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &actorID, nil
}

func appendAdminAudit(ctx context.Context, tx pgx.Tx, actorID *uuid.UUID, principal identity.Principal, organizationID *uuid.UUID, action, resourceType, resourceID string, summary map[string]any) error {
	if summary == nil {
		summary = map[string]any{}
	}
	summary["actor_issuer"] = principal.Issuer
	summary["actor_subject"] = principal.Subject
	body, err := json.Marshal(summary)
	if err != nil {
		return err
	}
	requestID := httpserver.RequestID(ctx)
	if requestID == "" {
		requestID = "internal"
	}
	_, err = tx.Exec(ctx, `
INSERT INTO audit_logs(id,actor_user_id,session_id,active_organization_id,action,resource_type,resource_id,result,request_id,trace_id,change_summary,occurred_at)
VALUES($1,$2,$3,$4,$5,$6,$7,'SUCCEEDED',$8,$8,$9,now())`, uuid.Must(uuid.NewV7()), actorID, principal.SessionID, organizationID, action, resourceType, resourceID, requestID, body)
	return err
}
