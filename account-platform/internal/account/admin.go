package account

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	platformauthorization "github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
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

type AdminUserFilter struct {
	Search                string
	Limit                 int
	ExcludeOrganizationID *uuid.UUID
	CursorUpdatedAt       *time.Time
	CursorID              *uuid.UUID
}

type AdminUserPage struct {
	Items      []AdminUser `json:"items"`
	NextCursor *string     `json:"next_cursor"`
}

var (
	ErrTokenOwnerCandidateCursorExpired = errors.New("token owner candidate cursor expired")
	ErrTokenOwnerCandidateRateLimited   = errors.New("token owner candidate query rate limited")
	ErrInvalidTokenOwnerCandidateCursor = errors.New("invalid token owner candidate cursor")
	ErrInvalidTokenOwnerCandidateFilter = errors.New("invalid token owner candidate filter")
	ErrInvalidTokenOwnerCandidateLimit  = errors.New("invalid token owner candidate limit")
)

type TokenOwnerCandidate struct {
	ID           uuid.UUID `json:"id"`
	Type         string    `json:"type"`
	DisplayLabel string    `json:"display_label"`
	Status       string    `json:"status"`
}

type tokenOwnerCandidateCursor struct {
	Version              int       `json:"version"`
	Resource             string    `json:"resource"`
	CallerService        string    `json:"caller_service"`
	CallerSubject        string    `json:"caller_subject"`
	ActorIssuer          string    `json:"actor_issuer"`
	ActorSubject         string    `json:"actor_subject"`
	AuthorizationVersion int64     `json:"authorization_version"`
	FilterFingerprint    string    `json:"filter_fingerprint"`
	ScanID               uuid.UUID `json:"scan_id"`
	HighWater            time.Time `json:"high_water"`
	OwnerType            string    `json:"owner_type"`
	OwnerID              uuid.UUID `json:"owner_id"`
	IssuedAt             time.Time `json:"issued_at"`
	ExpiresAt            time.Time `json:"expires_at"`
}

type TokenOwnerCandidateFilter struct {
	OwnerType      string
	OrganizationID *uuid.UUID
	Limit          int
	Cursor         *tokenOwnerCandidateCursor
	CursorErr      error
}

type TokenOwnerCandidateActor struct {
	Issuer        string
	Subject       string
	CallerService string
	CallerSubject string
}

type TokenOwnerCandidatePage struct {
	Items                []TokenOwnerCandidate `json:"items"`
	NextCursor           *string               `json:"next_cursor"`
	HasMore              bool                  `json:"has_more"`
	Limit                int                   `json:"limit"`
	AuthorizationVersion int64                 `json:"authorization_version"`
	next                 *tokenOwnerCandidateCursor
}

type AdminOrganization struct {
	ID          uuid.UUID `json:"organization_id"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	MemberCount int64     `json:"member_count"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type AdminOrganizationFilter struct {
	Search          string
	Status          string
	Limit           int
	CursorUpdatedAt *time.Time
	CursorID        *uuid.UUID
}

type AdminOrganizationPage struct {
	Items      []AdminOrganization `json:"items"`
	NextCursor *string             `json:"next_cursor"`
}

type AdminMemberFilter struct {
	Search         string
	Role           string
	Limit          int
	CursorJoinedAt *time.Time
	CursorID       *uuid.UUID
}

type AdminMemberPage struct {
	Items      []Member `json:"items"`
	NextCursor *string  `json:"next_cursor"`
}

type AdminOrganizationState struct {
	ID      uuid.UUID `json:"organization_id"`
	Status  string    `json:"status"`
	Version int64     `json:"version"`
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
	AdminUsers(context.Context, AdminUserFilter) (AdminUserPage, error)
	AdminUserMemberships(context.Context, uuid.UUID) ([]AdminUserMembership, error)
	AdminSetUserStatus(context.Context, identity.Principal, uuid.UUID, string) error
	AdminDeleteUser(context.Context, identity.Principal, uuid.UUID) error
	AdminOrganizations(context.Context, AdminOrganizationFilter) (AdminOrganizationPage, error)
	AdminCreateOrganization(context.Context, identity.Principal, uuid.UUID, string, string) (Organization, error)
	AdminRecordOrganizationCreationFailure(context.Context, identity.Principal, uuid.UUID, string, string) error
	AdminOrganizationState(context.Context, uuid.UUID) (AdminOrganizationState, error)
	AdminOrganizationDeletionBlockers(context.Context, uuid.UUID) ([]OrganizationLifecycleBlocker, error)
	AdminRenameOrganization(context.Context, identity.Principal, uuid.UUID, string) error
	AdminSetOrganizationStatus(context.Context, identity.Principal, uuid.UUID, string) error
	AdminDeleteOrganization(context.Context, identity.Principal, uuid.UUID, string) error
	AdminUnits(context.Context, uuid.UUID) (UnitTree, error)
	AdminUnitState(context.Context, uuid.UUID, uuid.UUID) (UnitMutationResult, error)
	AdminMembers(context.Context, uuid.UUID, AdminMemberFilter) (AdminMemberPage, error)
	AdminAddMember(context.Context, identity.Principal, uuid.UUID, uuid.UUID, []string, []uuid.UUID, string) (MembershipMutationResult, error)
	AdminReplaceMemberRoles(context.Context, identity.Principal, uuid.UUID, uuid.UUID, []string, []uuid.UUID) (Member, error)
	AdminRemoveMember(context.Context, identity.Principal, uuid.UUID, uuid.UUID, string) (MembershipMutationResult, error)
	AdminTransferOwnership(context.Context, identity.Principal, uuid.UUID, uuid.UUID, string) (OwnershipTransferResult, error)
	AdminCreateUnit(context.Context, identity.Principal, uuid.UUID, *uuid.UUID, string, string) (UnitMutationResult, error)
	AdminRenameUnit(context.Context, identity.Principal, uuid.UUID, uuid.UUID, string, string) (UnitMutationResult, error)
	AdminMoveUnit(context.Context, identity.Principal, uuid.UUID, uuid.UUID, *uuid.UUID, int64, string) (UnitMutationResult, error)
	AdminUnitDeletionBlockers(context.Context, uuid.UUID, uuid.UUID) ([]OrganizationLifecycleBlocker, error)
	AdminDeleteUnit(context.Context, identity.Principal, uuid.UUID, uuid.UUID, string) (UnitMutationResult, error)
	AdminAuditLogs(context.Context, AdminAuditFilter) ([]AdminAuditLog, error)
	AdminAuditLog(context.Context, uuid.UUID) (AdminAuditLog, error)
}

type tokenOwnerCandidateRepository interface {
	TokenOwnerCandidates(context.Context, TokenOwnerCandidateActor, TokenOwnerCandidateFilter) (TokenOwnerCandidatePage, error)
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

func parseTokenOwnerCandidateFilter(r *http.Request) (TokenOwnerCandidateFilter, error) {
	query := r.URL.Query()
	for key := range query {
		if !slices.Contains([]string{"actor_issuer", "actor_subject", "owner_type", "organization_id", "limit", "cursor"}, key) {
			return TokenOwnerCandidateFilter{}, ErrInvalidTokenOwnerCandidateFilter
		}
	}
	issuer, subject := strings.TrimSpace(query.Get("actor_issuer")), strings.TrimSpace(query.Get("actor_subject"))
	if issuer == "" || subject == "" || len(issuer) > 512 || len(subject) > 255 || strings.ContainsAny(issuer+subject, "\r\n") {
		return TokenOwnerCandidateFilter{}, ErrInvalidTokenOwnerCandidateFilter
	}
	filter := TokenOwnerCandidateFilter{
		OwnerType: strings.ToUpper(strings.TrimSpace(query.Get("owner_type"))),
		Limit:     20000,
	}
	if filter.OwnerType != "" && filter.OwnerType != "USER" && filter.OwnerType != "ORGANIZATION" && filter.OwnerType != "UNIT" {
		return TokenOwnerCandidateFilter{}, ErrInvalidTokenOwnerCandidateFilter
	}
	if raw := strings.TrimSpace(query.Get("organization_id")); raw != "" {
		organizationID, err := uuid.Parse(raw)
		if err != nil {
			return TokenOwnerCandidateFilter{}, ErrInvalidTokenOwnerCandidateFilter
		}
		filter.OrganizationID = &organizationID
	}
	if rawLimit := strings.TrimSpace(query.Get("limit")); rawLimit != "" {
		limit, err := strconv.Atoi(rawLimit)
		if err != nil || limit < 1 || limit > 50000 {
			return TokenOwnerCandidateFilter{}, ErrInvalidTokenOwnerCandidateLimit
		}
		filter.Limit = limit
	}
	return filter, nil
}

func tokenOwnerCandidateFilterFingerprint(filter TokenOwnerCandidateFilter) string {
	organizationID := ""
	if filter.OrganizationID != nil {
		organizationID = filter.OrganizationID.String()
	}
	body := strings.Join([]string{filter.OwnerType, organizationID, strconv.Itoa(filter.Limit)}, "\x00")
	digest := sha256.Sum256([]byte(body))
	return fmt.Sprintf("%x", digest[:])
}

func parseAdminUserFilter(r *http.Request) (AdminUserFilter, error) {
	filter := AdminUserFilter{Search: strings.TrimSpace(r.URL.Query().Get("q")), Limit: adminLimit(r)}
	excludeOrganizationID, err := optionalQueryUUID(r, "exclude_organization_id")
	if err != nil {
		return AdminUserFilter{}, err
	}
	filter.ExcludeOrganizationID = excludeOrganizationID
	cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
	if cursor == "" {
		return filter, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return AdminUserFilter{}, err
	}
	var value struct {
		UpdatedAt time.Time `json:"updated_at"`
		ID        uuid.UUID `json:"user_id"`
	}
	if err := json.Unmarshal(raw, &value); err != nil || value.UpdatedAt.IsZero() || value.ID == uuid.Nil {
		return AdminUserFilter{}, ErrConflict
	}
	value.UpdatedAt = value.UpdatedAt.UTC()
	filter.CursorUpdatedAt, filter.CursorID = &value.UpdatedAt, &value.ID
	return filter, nil
}

func encodeAdminUserCursor(item AdminUser) (string, error) {
	body, err := json.Marshal(map[string]any{"updated_at": item.UpdatedAt.UTC(), "user_id": item.ID})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(body), nil
}

func parseAdminOrganizationFilter(r *http.Request) (AdminOrganizationFilter, error) {
	filter := AdminOrganizationFilter{Search: strings.TrimSpace(r.URL.Query().Get("q")), Status: strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("status"))), Limit: adminLimit(r)}
	if filter.Status != "" && filter.Status != "ACTIVE" && filter.Status != "DISABLED" && filter.Status != "DELETED" {
		return AdminOrganizationFilter{}, ErrConflict
	}
	cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
	if cursor == "" {
		return filter, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return AdminOrganizationFilter{}, err
	}
	var value struct {
		UpdatedAt time.Time `json:"updated_at"`
		ID        uuid.UUID `json:"organization_id"`
	}
	if err := json.Unmarshal(raw, &value); err != nil || value.UpdatedAt.IsZero() || value.ID == uuid.Nil {
		return AdminOrganizationFilter{}, ErrConflict
	}
	value.UpdatedAt = value.UpdatedAt.UTC()
	filter.CursorUpdatedAt, filter.CursorID = &value.UpdatedAt, &value.ID
	return filter, nil
}

func encodeAdminOrganizationCursor(item AdminOrganization) (string, error) {
	body, err := json.Marshal(map[string]any{"updated_at": item.UpdatedAt.UTC(), "organization_id": item.ID})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(body), nil
}

func parseAdminMemberFilter(r *http.Request) (AdminMemberFilter, error) {
	filter := AdminMemberFilter{Search: strings.TrimSpace(r.URL.Query().Get("q")), Role: strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("role"))), Limit: adminLimit(r)}
	if filter.Role != "" {
		allowed := map[string]bool{"OWNER": true, "ORG_ADMIN": true, "ORG_MODEL_ADMIN": true, "BILLING_ADMIN": true, "AUDITOR": true, "MEMBER": true, "UNIT_ADMIN": true}
		if !allowed[filter.Role] {
			return AdminMemberFilter{}, ErrConflict
		}
	}
	cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
	if cursor == "" {
		return filter, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return AdminMemberFilter{}, err
	}
	var value struct {
		JoinedAt time.Time `json:"joined_at"`
		ID       uuid.UUID `json:"membership_id"`
	}
	if err := json.Unmarshal(raw, &value); err != nil || value.JoinedAt.IsZero() || value.ID == uuid.Nil {
		return AdminMemberFilter{}, ErrConflict
	}
	value.JoinedAt = value.JoinedAt.UTC()
	filter.CursorJoinedAt, filter.CursorID = &value.JoinedAt, &value.ID
	return filter, nil
}

func encodeAdminMemberCursor(item Member) (string, error) {
	body, err := json.Marshal(map[string]any{"joined_at": item.JoinedAt.UTC(), "membership_id": item.MembershipID})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(body), nil
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
	response := make(map[string]int64, 6)
	if platformauthorization.GrantedPermission(r.Context(), PermissionPlatformUsersRead) {
		response["users"] = result.Users
		response["active_users"] = result.ActiveUsers
		response["disabled_users"] = result.DisabledUsers
	}
	if platformauthorization.GrantedPermission(r.Context(), PermissionPlatformOrganizationsRead) {
		response["organizations"] = result.Organizations
		response["active_memberships"] = result.ActiveMemberships
		response["pending_invitations"] = result.PendingInvitations
	}
	api.WriteJSON(w, http.StatusOK, response)
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
	item, err := repository.AdminGrantRoleBinding(r.Context(), mustPrincipal(r.Context()), RoleBindingGrant{UserID: body.UserID, RoleCode: body.RoleCode, OrganizationID: body.OrganizationID, UnitID: body.UnitID, StartsAt: body.StartsAt, ExpiresAt: body.ExpiresAt, IdempotencyKey: body.IdempotencyKey, Reason: body.Reason})
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
	filter, err := parseAdminUserFilter(r)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_USER_FILTER", "user filters are invalid", httpserver.RequestID(r.Context()))
		return
	}
	if filter.ExcludeOrganizationID != nil && !platformauthorization.GrantedPermission(r.Context(), PermissionPlatformOrganizationsRead) {
		api.WriteError(w, http.StatusForbidden, "FORBIDDEN", "organization visibility is required for membership exclusion", httpserver.RequestID(r.Context()))
		return
	}
	page, err := repository.AdminUsers(r.Context(), filter)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, page)
}

func (s *Service) internalTokenOwnerCandidates(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(tokenOwnerCandidateRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "OWNER_DIRECTORY_UNAVAILABLE", "owner directory is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	workload, ok := serviceauth.FromContext(r.Context())
	if !ok || strings.TrimSpace(workload.Service) == "" || strings.TrimSpace(workload.Subject) == "" {
		api.WriteError(w, http.StatusForbidden, "SERVICE_AUTH_FORBIDDEN", "trusted service identity is required", httpserver.RequestID(r.Context()))
		return
	}
	if len(s.ownerCandidateCursorKey) < 32 {
		api.WriteError(w, http.StatusServiceUnavailable, "OWNER_DIRECTORY_UNAVAILABLE", "owner directory cursor signing is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	actor := TokenOwnerCandidateActor{
		Issuer:        strings.TrimSpace(r.URL.Query().Get("actor_issuer")),
		Subject:       strings.TrimSpace(r.URL.Query().Get("actor_subject")),
		CallerService: workload.Service,
		CallerSubject: workload.Subject,
	}
	filter, err := parseTokenOwnerCandidateFilter(r)
	if err != nil {
		status, code, message := http.StatusBadRequest, "INVALID_OWNER_CANDIDATE_FILTER", "owner candidate filters are invalid"
		if errors.Is(err, ErrInvalidTokenOwnerCandidateCursor) {
			code, message = "INVALID_CURSOR", "the owner candidate cursor is invalid"
		} else if errors.Is(err, ErrInvalidTokenOwnerCandidateLimit) {
			code, message = "INVALID_LIMIT", "owner candidate limit must be between 1 and 50000"
		}
		api.WriteError(w, status, code, message, httpserver.RequestID(r.Context()))
		return
	}
	if rawCursor := strings.TrimSpace(r.URL.Query().Get("cursor")); rawCursor != "" {
		if len(rawCursor) > 4096 {
			filter.CursorErr = ErrInvalidTokenOwnerCandidateCursor
		} else {
			cursor, cursorErr := decodeTokenOwnerCandidateCursor(rawCursor, s.ownerCandidateCursorKey, actor, filter, s.now())
			if cursorErr != nil {
				filter.CursorErr = cursorErr
			} else {
				filter.Cursor = cursor
			}
		}
	}
	page, err := repository.TokenOwnerCandidates(r.Context(), actor, filter)
	if err != nil {
		switch {
		case errors.Is(err, ErrTokenOwnerCandidateRateLimited):
			w.Header().Set("Retry-After", "60")
			api.WriteError(w, http.StatusTooManyRequests, "RATE_LIMITED", "the owner candidate query rate was exceeded", httpserver.RequestID(r.Context()))
		case errors.Is(err, ErrTokenOwnerCandidateCursorExpired):
			api.WriteError(w, http.StatusGone, "CURSOR_EXPIRED", "the owner candidate cursor no longer matches the current authorization", httpserver.RequestID(r.Context()))
		case errors.Is(err, ErrInvalidTokenOwnerCandidateCursor):
			api.WriteError(w, http.StatusBadRequest, "INVALID_CURSOR", "the owner candidate cursor is invalid", httpserver.RequestID(r.Context()))
		case errors.Is(err, ErrForbidden):
			api.WriteError(w, http.StatusForbidden, "PERMISSION_DENIED", "the requested permission is not granted", httpserver.RequestID(r.Context()))
		default:
			s.writeError(w, r, err)
		}
		return
	}
	if page.next != nil {
		cursor := *page.next
		cursor.Version = 1
		cursor.Resource = tokenOwnerCandidateCursorResource
		cursor.CallerService = actor.CallerService
		cursor.CallerSubject = actor.CallerSubject
		cursor.ActorIssuer = actor.Issuer
		cursor.ActorSubject = actor.Subject
		cursor.FilterFingerprint = tokenOwnerCandidateFilterFingerprint(filter)
		encoded, cursorErr := encodeTokenOwnerCandidateCursor(cursor, s.ownerCandidateCursorKey)
		if cursorErr != nil {
			api.WriteError(w, http.StatusServiceUnavailable, "OWNER_DIRECTORY_UNAVAILABLE", "owner directory cursor signing is unavailable", httpserver.RequestID(r.Context()))
			return
		}
		page.NextCursor = &encoded
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, page)
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
	filter, err := parseAdminOrganizationFilter(r)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_ORGANIZATION_FILTER", "organization filters are invalid", httpserver.RequestID(r.Context()))
		return
	}
	page, err := repository.AdminOrganizations(r.Context(), filter)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, page)
}

func (s *Service) adminCreateOrganization(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	var body struct {
		Name           string    `json:"name"`
		OwnerUserID    uuid.UUID `json:"owner_user_id"`
		IdempotencyKey string    `json:"idempotency_key"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || body.OwnerUserID == uuid.Nil || strings.TrimSpace(body.Name) == "" {
		_ = repository.AdminRecordOrganizationCreationFailure(r.Context(), mustPrincipal(r.Context()), body.OwnerUserID, body.Name, "INVALID_REQUEST")
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "organization name and owner are required", httpserver.RequestID(r.Context()))
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		idempotencyKey = strings.TrimSpace(body.IdempotencyKey)
	}
	principal := mustPrincipal(r.Context())
	if idempotencyKey == "" {
		fallback := sha256.Sum256([]byte(principal.Issuer + "\x00" + principal.SessionID + "\x00" + body.OwnerUserID.String() + "\x00" + strings.TrimSpace(body.Name)))
		idempotencyKey = fmt.Sprintf("portal-session:%x", fallback[:16])
	}
	if len(idempotencyKey) < 8 || len(idempotencyKey) > 120 {
		_ = repository.AdminRecordOrganizationCreationFailure(r.Context(), principal, body.OwnerUserID, body.Name, "INVALID_IDEMPOTENCY_KEY")
		api.WriteError(w, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "an idempotency key between 8 and 120 characters is required", httpserver.RequestID(r.Context()))
		return
	}
	organization, err := repository.AdminCreateOrganization(r.Context(), principal, body.OwnerUserID, body.Name, idempotencyKey)
	if err != nil {
		_ = repository.AdminRecordOrganizationCreationFailure(r.Context(), principal, body.OwnerUserID, body.Name, organizationCreationFailureCode(err))
		s.writeError(w, r, err)
		return
	}
	if len(s.organizationLifecycle) > 0 {
		assessment, lifecycleErr := s.applyOrganizationLifecycleProjection(r.Context(), organization.ID, "ACTIVE", organization.Version)
		if lifecycleErr != nil || !assessment.Ready {
			api.WriteError(w, http.StatusServiceUnavailable, "ORGANIZATION_LIFECYCLE_UNAVAILABLE", "the organization was created but dependent lifecycle activation must be retried with the same idempotency key", httpserver.RequestID(r.Context()))
			return
		}
	}
	api.WriteJSON(w, http.StatusCreated, organization)
}

func organizationCreationFailureCode(err error) string {
	switch {
	case errors.Is(err, ErrNotFound):
		return "OWNER_NOT_FOUND"
	case errors.Is(err, ErrForbidden):
		return "PERMISSION_DENIED"
	case errors.Is(err, ErrConflict):
		return "NAME_OR_IDEMPOTENCY_CONFLICT"
	default:
		return "INTERNAL_ERROR"
	}
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
	tree, err := repository.AdminUnits(r.Context(), organizationID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, tree)
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
	filter, err := parseAdminMemberFilter(r)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_ORGANIZATION_MEMBER_FILTER", "organization member filters are invalid", httpserver.RequestID(r.Context()))
		return
	}
	page, err := repository.AdminMembers(r.Context(), organizationID, filter)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, page)
}

type adminMemberRolesBody struct {
	UserID           uuid.UUID   `json:"user_id"`
	Roles            []string    `json:"roles"`
	UnitAdminUnitIDs []uuid.UUID `json:"unit_admin_unit_ids"`
	IdempotencyKey   string      `json:"idempotency_key"`
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
	idempotencyKey := organizationUnitIdempotencyKey(r, body.IdempotencyKey, "admin-member-add:"+organizationID.String()+":"+body.UserID.String())
	result, err := repository.AdminAddMember(r.Context(), mustPrincipal(r.Context()), organizationID, body.UserID, body.Roles, body.UnitAdminUnitIDs, idempotencyKey)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.applyOrganizationMemberLifecycle(r.Context(), result, "ACTIVE"); err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "ORGANIZATION_MEMBER_LIFECYCLE_UNAVAILABLE", "the member was added locally but grant activation must be retried with the same idempotency key", httpserver.RequestID(r.Context()))
		return
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
	}
	api.WriteJSON(w, status, result)
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
	idempotencyKey := organizationUnitIdempotencyKey(r, r.URL.Query().Get("idempotency_key"), "admin-member-remove:"+organizationID.String()+":"+userID.String())
	result, err := repository.AdminRemoveMember(r.Context(), mustPrincipal(r.Context()), organizationID, userID, idempotencyKey)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.applyOrganizationMemberLifecycle(r.Context(), result, "REVOKED"); err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "ORGANIZATION_MEMBER_LIFECYCLE_UNAVAILABLE", "the membership was revoked locally but grant suspension must be retried with the same idempotency key", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
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
		IdempotencyKey string    `json:"idempotency_key"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	if body.NewOwnerUserID == uuid.Nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "new owner is required", httpserver.RequestID(r.Context()))
		return
	}
	idempotencyKey := organizationUnitIdempotencyKey(r, body.IdempotencyKey, "admin-ownership-transfer:"+organizationID.String()+":"+body.NewOwnerUserID.String())
	result, err := repository.AdminTransferOwnership(r.Context(), mustPrincipal(r.Context()), organizationID, body.NewOwnerUserID, idempotencyKey)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
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
		Name           string     `json:"name"`
		ParentID       *uuid.UUID `json:"parent_id"`
		IdempotencyKey string     `json:"idempotency_key"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	idempotencyKey := organizationUnitIdempotencyKey(r, body.IdempotencyKey, "admin-unit-create:"+organizationID.String()+":"+strings.TrimSpace(body.Name)+":"+fmt.Sprint(body.ParentID))
	if !validOrganizationUnitIdempotencyKey(idempotencyKey) {
		api.WriteError(w, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "an idempotency key between 8 and 120 characters is required", httpserver.RequestID(r.Context()))
		return
	}
	result, err := repository.AdminCreateUnit(r.Context(), mustPrincipal(r.Context()), organizationID, body.ParentID, body.Name, idempotencyKey)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if _, err := s.applyOrganizationUnitLifecycle(r.Context(), organizationID, result.Unit.ID, "ACTIVE", result.StructureVersion); err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "ORGANIZATION_UNIT_LIFECYCLE_UNAVAILABLE", "the unit was created but dependent lifecycle activation must be retried with the same idempotency key", httpserver.RequestID(r.Context()))
		return
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
	}
	api.WriteJSON(w, status, result)
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
		Name           string `json:"name"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	idempotencyKey := organizationUnitIdempotencyKey(r, body.IdempotencyKey, "admin-unit-rename:"+organizationID.String()+":"+unitID.String()+":"+strings.TrimSpace(body.Name))
	result, err := repository.AdminRenameUnit(r.Context(), mustPrincipal(r.Context()), organizationID, unitID, body.Name, idempotencyKey)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) adminMoveUnit(w http.ResponseWriter, r *http.Request) {
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
		ParentID                 *uuid.UUID `json:"parent_id"`
		ExpectedStructureVersion int64      `json:"expected_structure_version"`
		IdempotencyKey           string     `json:"idempotency_key"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid organization unit move request", httpserver.RequestID(r.Context()))
		return
	}
	idempotencyKey := organizationUnitIdempotencyKey(r, body.IdempotencyKey, "admin-unit-move:"+organizationID.String()+":"+unitID.String()+":"+fmt.Sprint(body.ParentID)+":"+fmt.Sprint(body.ExpectedStructureVersion))
	if body.ExpectedStructureVersion < 1 || !validOrganizationUnitIdempotencyKey(idempotencyKey) {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "expected_structure_version and a valid idempotency key are required", httpserver.RequestID(r.Context()))
		return
	}
	result, err := repository.AdminMoveUnit(r.Context(), mustPrincipal(r.Context()), organizationID, unitID, body.ParentID, body.ExpectedStructureVersion, idempotencyKey)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
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
	idempotencyKey := organizationUnitIdempotencyKey(r, r.URL.Query().Get("idempotency_key"), "admin-unit-delete:"+organizationID.String()+":"+unitID.String())
	state, err := repository.AdminUnitState(r.Context(), organizationID, unitID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if state.Unit.Status != "DELETED" {
		blockers, err := repository.AdminUnitDeletionBlockers(r.Context(), organizationID, unitID)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		if len(blockers) > 0 {
			s.writeOrganizationUnitLifecycleFailure(w, r, OrganizationUnitDeletionAssessment{OrganizationID: organizationID, UnitID: unitID, Status: "DELETION_PENDING", StructureVersion: state.StructureVersion, Blockers: blockers})
			return
		}
		assessment, err := s.applyOrganizationUnitLifecycle(r.Context(), organizationID, unitID, "DELETION_PENDING", state.StructureVersion)
		if err != nil {
			api.WriteError(w, http.StatusServiceUnavailable, "ORGANIZATION_UNIT_LIFECYCLE_UNAVAILABLE", "organization unit deletion facts could not be confirmed", httpserver.RequestID(r.Context()))
			return
		}
		if !assessment.Ready {
			s.writeOrganizationUnitLifecycleFailure(w, r, assessment)
			return
		}
	}
	result, err := repository.AdminDeleteUnit(r.Context(), mustPrincipal(r.Context()), organizationID, unitID, idempotencyKey)
	if err != nil {
		var blocked *OrganizationUnitBlockedError
		if errors.As(err, &blocked) {
			s.writeOrganizationUnitLifecycleFailure(w, r, OrganizationUnitDeletionAssessment{OrganizationID: organizationID, UnitID: unitID, Status: "DELETION_PENDING", StructureVersion: state.StructureVersion, Blockers: blocked.Blockers})
			return
		}
		s.writeError(w, r, err)
		return
	}
	assessment, err := s.applyOrganizationUnitLifecycle(r.Context(), organizationID, unitID, "DELETED", result.StructureVersion)
	if err != nil || !assessment.Ready {
		api.WriteError(w, http.StatusServiceUnavailable, "ORGANIZATION_UNIT_LIFECYCLE_UNAVAILABLE", "the unit was deleted locally but dependent lifecycle finalization must be retried with the same idempotency key", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) writeOrganizationUnitLifecycleFailure(w http.ResponseWriter, r *http.Request, assessment OrganizationUnitDeletionAssessment) {
	api.WriteJSON(w, http.StatusConflict, api.Error{Code: "ORGANIZATION_UNIT_LIFECYCLE_BLOCKED", Message: "organization unit deletion is blocked by associated resources", RequestID: httpserver.RequestID(r.Context()), Details: map[string]any{"organization_id": assessment.OrganizationID, "unit_id": assessment.UnitID, "requested_status": assessment.Status, "structure_version": assessment.StructureVersion, "blockers": assessment.Blockers}})
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
		state, stateErr := repository.AdminOrganizationState(r.Context(), resourceID)
		if stateErr != nil {
			s.writeError(w, r, stateErr)
			return
		}
		if state.Status == body.Status {
			if len(s.organizationLifecycle) > 0 {
				assessment, lifecycleErr := s.applyOrganizationLifecycleProjection(r.Context(), resourceID, body.Status, state.Version)
				if lifecycleErr != nil || !assessment.Ready {
					s.writeOrganizationLifecycleFailure(w, r, assessment, lifecycleErr)
					return
				}
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		nextVersion := state.Version + 1
		if len(s.organizationLifecycle) > 0 {
			assessment, lifecycleErr := s.applyOrganizationLifecycleProjection(r.Context(), resourceID, body.Status, nextVersion)
			if lifecycleErr != nil || !assessment.Ready {
				s.writeOrganizationLifecycleFailure(w, r, assessment, lifecycleErr)
				return
			}
		}
		err = repository.AdminSetOrganizationStatus(r.Context(), principal, resourceID, body.Status)
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) adminDeleteUser(w http.ResponseWriter, r *http.Request) {
	s.adminDelete(w, r, "user", "")
}

func (s *Service) adminDeleteOrganization(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := api.DecodeJSON(w, r, &body); err != nil {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "request body must be valid JSON", httpserver.RequestID(r.Context()))
			return
		}
		body.Reason = strings.TrimSpace(body.Reason)
		if body.Reason == "" || len([]rune(body.Reason)) > 500 {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "reason must contain between 1 and 500 characters", httpserver.RequestID(r.Context()))
			return
		}
	}
	s.adminDelete(w, r, "organization", body.Reason)
}

func (s *Service) adminOrganizationDeletionReadiness(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	readiness, err := s.assessOrganizationDeletion(r.Context(), repository, organizationID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			s.writeError(w, r, err)
			return
		}
		api.WriteError(w, http.StatusServiceUnavailable, "ORGANIZATION_LIFECYCLE_UNAVAILABLE", "organization lifecycle facts could not be confirmed", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteJSON(w, http.StatusOK, readiness)
}

func (s *Service) adminDelete(w http.ResponseWriter, r *http.Request, resource, reason string) {
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
		state, stateErr := repository.AdminOrganizationState(r.Context(), resourceID)
		if stateErr != nil {
			s.writeError(w, r, stateErr)
			return
		}
		if state.Status == "DELETED" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if state.Status != "DISABLED" {
			s.writeOrganizationLifecycleFailure(w, r, OrganizationLifecycleAssessment{OrganizationID: resourceID, Status: "DELETED", AccountVersion: state.Version + 1, Ready: false, Blockers: []OrganizationLifecycleBlocker{{Code: "ORGANIZATION_NOT_DISABLED", Domain: "ACCOUNT", ResourceType: "organization", ResourceID: resourceID.String()}}}, nil)
			return
		}
		blockers, blockerErr := repository.AdminOrganizationDeletionBlockers(r.Context(), resourceID)
		if blockerErr != nil {
			s.writeError(w, r, blockerErr)
			return
		}
		if len(blockers) > 0 {
			s.writeOrganizationLifecycleFailure(w, r, OrganizationLifecycleAssessment{OrganizationID: resourceID, Status: "DELETED", AccountVersion: state.Version + 1, Ready: false, Blockers: blockers}, nil)
			return
		}
		if len(s.organizationLifecycle) > 0 {
			currentAssessment, currentErr := s.applyOrganizationLifecycleProjection(r.Context(), resourceID, "DISABLED", state.Version)
			if currentErr != nil || !currentAssessment.Ready {
				s.writeOrganizationLifecycleFailure(w, r, currentAssessment, currentErr)
				return
			}
			preflightAssessment, preflightErr := s.preflightOrganizationLifecycleProjection(r.Context(), resourceID, "DELETED", state.Version+1)
			if preflightErr != nil || !preflightAssessment.Ready {
				s.writeOrganizationLifecycleFailure(w, r, preflightAssessment, preflightErr)
				return
			}
			assessment, lifecycleErr := s.applyOrganizationLifecycleProjection(r.Context(), resourceID, "DELETED", state.Version+1)
			if lifecycleErr != nil || !assessment.Ready {
				s.writeOrganizationLifecycleFailure(w, r, assessment, lifecycleErr)
				return
			}
		}
		err = repository.AdminDeleteOrganization(r.Context(), principal, resourceID, reason)
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) writeOrganizationLifecycleFailure(w http.ResponseWriter, r *http.Request, assessment OrganizationLifecycleAssessment, err error) {
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "ORGANIZATION_LIFECYCLE_UNAVAILABLE", "organization lifecycle facts could not be confirmed", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteJSON(w, http.StatusConflict, api.Error{Code: "ORGANIZATION_LIFECYCLE_BLOCKED", Message: "organization lifecycle is blocked by unconverged facts", RequestID: httpserver.RequestID(r.Context()), Details: map[string]any{"organization_id": assessment.OrganizationID, "requested_status": assessment.Status, "blockers": assessment.Blockers}})
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

func (r *PostgresRepository) AdminUsers(ctx context.Context, filter AdminUserFilter) (AdminUserPage, error) {
	search := strings.ToLower(strings.TrimSpace(filter.Search))
	pattern := "%" + search + "%"
	limit := filter.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.pool.Query(ctx, `
SELECT u.id,u.user_code,u.username,u.display_name,u.email,u.status,
       (SELECT count(*) FROM memberships m WHERE m.user_id=u.id AND m.status='ACTIVE'),
       (SELECT max(s.last_seen_at) FROM user_sessions s WHERE s.user_id=u.id),
       u.created_at,u.updated_at
FROM users u
WHERE u.status<>'DELETED' AND ($1='' OR lower(COALESCE(u.username,'')) LIKE $2 OR lower(COALESCE(u.display_name,'')) LIKE $2 OR u.id::text=$1)
	AND ($3::uuid IS NULL OR NOT EXISTS(SELECT 1 FROM memberships m WHERE m.organization_id=$3 AND m.user_id=u.id AND m.status<>'REVOKED'))
	AND ($4::timestamptz IS NULL OR u.updated_at<$4 OR (u.updated_at=$4 AND u.id<$5::uuid))
ORDER BY u.updated_at DESC,u.id DESC LIMIT $6`, search, pattern, filter.ExcludeOrganizationID, filter.CursorUpdatedAt, filter.CursorID, limit+1)
	if err != nil {
		return AdminUserPage{}, err
	}
	defer rows.Close()
	items := make([]AdminUser, 0, limit+1)
	for rows.Next() {
		var item AdminUser
		if err := rows.Scan(&item.ID, &item.UserCode, &item.Username, &item.DisplayName, &item.Email, &item.Status, &item.OrganizationCount, &item.LastSeenAt, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return AdminUserPage{}, err
		}
		item.UserCode = FormatUserCode(item.UserCode)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return AdminUserPage{}, err
	}
	page := AdminUserPage{Items: items}
	if len(items) > limit {
		cursor, err := encodeAdminUserCursor(items[limit-1])
		if err != nil {
			return AdminUserPage{}, err
		}
		page.Items = items[:limit]
		page.NextCursor = &cursor
	}
	return page, nil
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

func (r *PostgresRepository) AdminOrganizations(ctx context.Context, filter AdminOrganizationFilter) (AdminOrganizationPage, error) {
	search := strings.ToLower(strings.TrimSpace(filter.Search))
	pattern := "%" + search + "%"
	limit := filter.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.pool.Query(ctx, `
SELECT o.id,o.name,o.status,count(m.id) FILTER (WHERE m.status='ACTIVE'),o.created_at,o.updated_at
FROM organizations o LEFT JOIN memberships m ON m.organization_id=o.id
WHERE ($1='' OR lower(o.name) LIKE $2 OR o.id::text=$1)
  AND ($3='' AND o.status<>'DELETED' OR o.status=$3)
  AND ($4::timestamptz IS NULL OR o.updated_at<$4 OR (o.updated_at=$4 AND o.id<$5::uuid))
GROUP BY o.id ORDER BY o.updated_at DESC,o.id DESC LIMIT $6`, search, pattern, filter.Status, filter.CursorUpdatedAt, filter.CursorID, limit+1)
	if err != nil {
		return AdminOrganizationPage{}, err
	}
	defer rows.Close()
	items := make([]AdminOrganization, 0, limit+1)
	for rows.Next() {
		var item AdminOrganization
		if err := rows.Scan(&item.ID, &item.Name, &item.Status, &item.MemberCount, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return AdminOrganizationPage{}, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return AdminOrganizationPage{}, err
	}
	page := AdminOrganizationPage{Items: items, NextCursor: nil}
	if len(items) > limit {
		cursor, err := encodeAdminOrganizationCursor(items[limit-1])
		if err != nil {
			return AdminOrganizationPage{}, err
		}
		page.Items = items[:limit]
		page.NextCursor = &cursor
	}
	return page, nil
}

func (r *PostgresRepository) AdminCreateOrganization(ctx context.Context, principal identity.Principal, ownerUserID uuid.UUID, name, idempotencyKey string) (Organization, error) {
	name = strings.TrimSpace(name)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if ownerUserID == uuid.Nil || name == "" || len([]rune(name)) > 120 || len(idempotencyKey) < 8 || len(idempotencyKey) > 120 {
		return Organization{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Organization{}, err
	}
	defer tx.Rollback(ctx)
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return Organization{}, err
	}
	if actorID == nil {
		return Organization{}, ErrForbidden
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "organization-create:"+actorID.String()+":"+idempotencyKey); err != nil {
		return Organization{}, err
	}
	requestHash := fmt.Sprintf("%x", sha256.Sum256([]byte(ownerUserID.String()+"\x00"+name)))
	var existingID uuid.UUID
	var existingHash string
	err = tx.QueryRow(ctx, `SELECT organization_id,request_hash FROM organization_creation_requests WHERE actor_user_id=$1 AND idempotency_key=$2`, *actorID, idempotencyKey).Scan(&existingID, &existingHash)
	if err == nil {
		if existingHash != requestHash {
			return Organization{}, ErrConflict
		}
		var existing Organization
		if err := tx.QueryRow(ctx, `SELECT id,name,status,version FROM organizations WHERE id=$1`, existingID).Scan(&existing.ID, &existing.Name, &existing.Status, &existing.Version); err != nil {
			return Organization{}, err
		}
		existing.Roles = []string{"OWNER"}
		return existing, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
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
	// The deferred single-owner constraint evaluates effective bindings against
	// PostgreSQL's transaction timestamp. Use that same timestamp for an
	// immediately effective OWNER grant; an application timestamp captured
	// after BEGIN would otherwise make the grant appear future-dated at COMMIT.
	if _, err := tx.Exec(ctx, `INSERT INTO role_bindings(id,membership_id,role,created_by,created_at,starts_at,grant_source) VALUES($1,$2,'OWNER',$3,$4,transaction_timestamp(),'ORGANIZATION_CREATION')`, uuid.Must(uuid.NewV7()), membershipID, ownerUserID, now); err != nil {
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
	if _, err := tx.Exec(ctx, `INSERT INTO organization_creation_requests(actor_user_id,idempotency_key,request_hash,organization_id,created_at) VALUES($1,$2,$3,$4,$5)`, *actorID, idempotencyKey, requestHash, organization.ID, now); err != nil {
		return Organization{}, mapConstraintError(err)
	}
	return organization, tx.Commit(ctx)
}

func (r *PostgresRepository) AdminOrganizationState(ctx context.Context, organizationID uuid.UUID) (AdminOrganizationState, error) {
	var state AdminOrganizationState
	err := r.pool.QueryRow(ctx, `SELECT id,status,version FROM organizations WHERE id=$1`, organizationID).Scan(&state.ID, &state.Status, &state.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminOrganizationState{}, ErrNotFound
	}
	return state, err
}

func (r *PostgresRepository) AdminRecordOrganizationCreationFailure(ctx context.Context, principal identity.Principal, ownerUserID uuid.UUID, name, reasonCode string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return err
	}
	if err := appendAdminAuditResult(ctx, tx, actorID, principal, nil, "admin.organization.create", "organization", "pending", "FAILED", map[string]any{"name": strings.TrimSpace(name), "owner_user_id": ownerUserID, "reason_code": strings.TrimSpace(reasonCode)}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) AdminOrganizationDeletionBlockers(ctx context.Context, organizationID uuid.UUID) ([]OrganizationLifecycleBlocker, error) {
	rows, err := r.pool.Query(ctx, `
SELECT code,resource_type,resource_id FROM (
  SELECT 'PENDING_INVITATION' code,'invitation' resource_type,id::text resource_id
  FROM invitations WHERE organization_id=$1 AND status='PENDING' AND expires_at>now()
  UNION ALL
  SELECT 'PENDING_APPROVAL_REQUEST','approval_request',id::text
  FROM approval_requests
  WHERE target_resource_type='organization' AND target_resource_id=$1::text
    AND status IN ('DRAFT','SUBMITTED','APPROVED','EXECUTING')
) blockers ORDER BY code,resource_id`, organizationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	blockers := make([]OrganizationLifecycleBlocker, 0)
	for rows.Next() {
		var blocker OrganizationLifecycleBlocker
		blocker.Domain = "ACCOUNT"
		if err := rows.Scan(&blocker.Code, &blocker.ResourceType, &blocker.ResourceID); err != nil {
			return nil, err
		}
		blockers = append(blockers, blocker)
	}
	return blockers, rows.Err()
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

func (r *PostgresRepository) AdminUnits(ctx context.Context, organizationID uuid.UUID) (UnitTree, error) {
	var structureVersion int64
	if err := r.pool.QueryRow(ctx, `SELECT structure_version FROM organizations WHERE id=$1 AND status<>'DELETED'`, organizationID).Scan(&structureVersion); errors.Is(err, pgx.ErrNoRows) {
		return UnitTree{}, ErrNotFound
	} else if err != nil {
		return UnitTree{}, err
	}
	rows, err := r.pool.Query(ctx, `SELECT id,organization_id,parent_id,name,status,version FROM organization_units WHERE organization_id=$1 ORDER BY created_at,id`, organizationID)
	if err != nil {
		return UnitTree{}, err
	}
	defer rows.Close()
	items := make([]Unit, 0)
	for rows.Next() {
		var item Unit
		if err := rows.Scan(&item.ID, &item.OrganizationID, &item.ParentID, &item.Name, &item.Status, &item.Version); err != nil {
			return UnitTree{}, err
		}
		items = append(items, item)
	}
	return UnitTree{Items: items, StructureVersion: structureVersion}, rows.Err()
}

func (r *PostgresRepository) AdminMembers(ctx context.Context, organizationID uuid.UUID, filter AdminMemberFilter) (AdminMemberPage, error) {
	var exists bool
	if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organizations WHERE id=$1 AND status<>'DELETED')`, organizationID).Scan(&exists); err != nil {
		return AdminMemberPage{}, err
	}
	if !exists {
		return AdminMemberPage{}, ErrNotFound
	}
	search := strings.ToLower(strings.TrimSpace(filter.Search))
	pattern := "%" + search + "%"
	limit := filter.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.pool.Query(ctx, `
SELECT m.id,m.user_id,u.user_code,u.display_name,u.email,m.status,m.version,m.joined_at,
       ARRAY(SELECT DISTINCT rb.role FROM role_bindings rb WHERE rb.membership_id=m.id AND rb.revoked_at IS NULL AND rb.starts_at<=now() AND (rb.expires_at IS NULL OR rb.expires_at>now()) ORDER BY rb.role),
       ARRAY(SELECT mu.unit_id FROM membership_units mu WHERE mu.membership_id=m.id ORDER BY mu.unit_id),
       ARRAY(SELECT rb.unit_id FROM role_bindings rb WHERE rb.membership_id=m.id AND rb.role='UNIT_ADMIN' AND rb.revoked_at IS NULL AND rb.starts_at<=now() AND (rb.expires_at IS NULL OR rb.expires_at>now()) ORDER BY rb.unit_id)
FROM memberships m JOIN users u ON u.id=m.user_id
WHERE m.organization_id=$1 AND m.status<>'REVOKED' AND u.status='ACTIVE'
  AND ($2='' OR lower(COALESCE(u.display_name,'')) LIKE $3 OR lower(u.user_code) LIKE $3 OR u.id::text=$2)
  AND ($4='' OR EXISTS(SELECT 1 FROM role_bindings rb WHERE rb.membership_id=m.id AND rb.role=$4 AND rb.revoked_at IS NULL AND rb.starts_at<=now() AND (rb.expires_at IS NULL OR rb.expires_at>now())))
  AND ($5::timestamptz IS NULL OR m.joined_at<$5 OR (m.joined_at=$5 AND m.id<$6::uuid))
ORDER BY m.joined_at DESC,m.id DESC LIMIT $7`, organizationID, search, pattern, filter.Role, filter.CursorJoinedAt, filter.CursorID, limit+1)
	if err != nil {
		return AdminMemberPage{}, err
	}
	defer rows.Close()
	items := make([]Member, 0, limit+1)
	for rows.Next() {
		var item Member
		if err := rows.Scan(&item.MembershipID, &item.UserID, &item.UserCode, &item.DisplayName, &item.Email, &item.Status, &item.Version, &item.JoinedAt, &item.Roles, &item.UnitIDs, &item.UnitAdminIDs); err != nil {
			return AdminMemberPage{}, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return AdminMemberPage{}, err
	}
	page := AdminMemberPage{Items: items, NextCursor: nil}
	if len(items) > limit {
		cursor, err := encodeAdminMemberCursor(items[limit-1])
		if err != nil {
			return AdminMemberPage{}, err
		}
		page.Items = items[:limit]
		page.NextCursor = &cursor
	}
	return page, nil
}

func normalizeAdminMemberRoles(roles []string, unitAdminIDs []uuid.UUID) ([]string, []uuid.UUID, error) {
	allowed := map[string]bool{"ORG_ADMIN": true, "ORG_MODEL_ADMIN": true, "BILLING_ADMIN": true, "AUDITOR": true, "MEMBER": true}
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
		if _, err := tx.Exec(ctx, `INSERT INTO role_bindings(id,membership_id,role,created_by,created_at,starts_at,grant_source) VALUES($1,$2,$3,$4,$5,$5,'ADMIN')`, uuid.Must(uuid.NewV7()), membershipID, role, actorID, now); err != nil {
			return 0, mapConstraintError(err)
		}
	}
	for _, unitID := range unitAdminIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO role_bindings(id,membership_id,role,unit_id,created_by,created_at,starts_at,grant_source) VALUES($1,$2,'UNIT_ADMIN',$3,$4,$5,$5,'ADMIN')`, uuid.Must(uuid.NewV7()), membershipID, unitID, actorID, now); err != nil {
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
	var currentVersion int64
	err = tx.QueryRow(ctx, `SELECT status,version FROM organizations WHERE id=$1 FOR UPDATE`, organizationID).Scan(&currentStatus, &currentVersion)
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
		if _, err := tx.Exec(ctx, `UPDATE user_active_contexts SET context_type='PERSONAL',organization_id=NULL,context_version=context_version+1,updated_at=$2 WHERE organization_id=$1`, organizationID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE user_sessions SET active_context_type='PERSONAL',active_organization_id=NULL,membership_version=membership_version+1,last_seen_at=$2 WHERE active_organization_id=$1`, organizationID, now); err != nil {
			return err
		}
	}
	if err := appendAdminAudit(ctx, tx, actorID, principal, &organizationID, "admin.organization.status.update", "organization", organizationID.String(), map[string]any{"from": currentStatus, "to": status}); err != nil {
		return err
	}
	if err := appendOutbox(ctx, tx, "account.organization.status-changed.v1", organizationID, currentVersion+1, map[string]any{"organization_id": organizationID, "from_status": currentStatus, "status": status, "version": currentVersion + 1}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) AdminDeleteOrganization(ctx context.Context, principal identity.Principal, organizationID uuid.UUID, reason string) error {
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
	var currentVersion int64
	err = tx.QueryRow(ctx, `SELECT status,version FROM organizations WHERE id=$1 FOR UPDATE`, organizationID).Scan(&status, &currentVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if status == "DELETED" {
		return tx.Commit(ctx)
	}
	if status != "DISABLED" {
		return ErrConflict
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
	if _, err := tx.Exec(ctx, `UPDATE user_active_contexts SET context_type='PERSONAL',organization_id=NULL,context_version=context_version+1,updated_at=$2 WHERE organization_id=$1`, organizationID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE organization_units SET status='DISABLED',name='Deleted unit '||left(id::text,8),version=version+1,updated_at=$2 WHERE organization_id=$1 AND status<>'DELETED'`, organizationID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE invitations SET status=CASE WHEN status='PENDING' THEN 'REVOKED' ELSE status END,email_normalized='deleted+'||left(id::text,8)||'@invalid.local',updated_at=$2 WHERE organization_id=$1`, organizationID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE organizations SET status='DELETED',name='Deleted organization '||left(id::text,8),version=version+1,deleted_at=$2,updated_at=$2 WHERE id=$1`, organizationID, now); err != nil {
		return err
	}
	summary := map[string]any{"from": status, "to": "DELETED"}
	if reason != "" {
		summary["reason"] = reason
	}
	if err := appendAdminAudit(ctx, tx, actorID, principal, &organizationID, "admin.organization.delete", "organization", organizationID.String(), summary); err != nil {
		return err
	}
	if err := appendOutbox(ctx, tx, "account.organization.deleted.v1", organizationID, currentVersion+1, map[string]any{"organization_id": organizationID, "status": "DELETED", "version": currentVersion + 1}); err != nil {
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
	return appendAdminAuditResult(ctx, tx, actorID, principal, organizationID, action, resourceType, resourceID, "SUCCEEDED", summary)
}

func appendAdminAuditResult(ctx context.Context, tx pgx.Tx, actorID *uuid.UUID, principal identity.Principal, organizationID *uuid.UUID, action, resourceType, resourceID, result string, summary map[string]any) error {
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
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$9,$10,now())`, uuid.Must(uuid.NewV7()), actorID, principal.SessionID, organizationID, action, resourceType, resourceID, result, requestID, body)
	return err
}
