package account

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	PermissionPlatformRolesRead                  = "platform.roles.read"
	PermissionPlatformRolesAssign                = "platform.roles.assign"
	PermissionPlatformUsersRead                  = "platform.users.read"
	PermissionPlatformUsersStatusManage          = "platform.users.status.manage"
	PermissionPlatformAccountClosuresRead        = "platform.account-closures.read"
	PermissionPlatformAccountClosuresRepair      = "platform.account-closures.repair"
	PermissionPlatformOrganizationsRead          = "platform.organizations.read"
	PermissionPlatformOrganizationsCreate        = "platform.organizations.create"
	PermissionPlatformOrganizationsStatusManage  = "platform.organizations.status.manage"
	PermissionPlatformOrganizationsDelete        = "platform.organizations.delete"
	PermissionPlatformOrganizationsOwnerTransfer = "platform.organizations.owner.transfer"
	PermissionPlatformAuditRead                  = "platform.audit.read"
	PermissionPlatformAuditExport                = "platform.audit.export"
	PermissionPlatformAuditPIIExport             = "platform.audit.pii.export"
	PermissionPlatformSystemStatusRead           = "platform.system.status.read"
	PermissionPlatformRegistrationsRepair        = "platform.registrations.repair"
	PermissionPlatformProfileChangesRepair       = "platform.profile-changes.repair"
	PermissionPersonalSessionLogin               = "personal.session.login"
	PermissionPersonalProfileRead                = "personal.profile.read"
	PermissionPersonalProfileUpdate              = "personal.profile.update"
	PermissionPersonalSecurityManage             = "personal.security.manage"
	PermissionPersonalContextSwitch              = "personal.context.switch"
	PermissionPersonalAccountClosureRequest      = "personal.account.close.request"
	PermissionPersonalAccountClosureRead         = "personal.account.close.read"
	PermissionPersonalInvitationsRead            = "personal.invitations.read"
	PermissionPersonalInvitationsAccept          = "personal.invitations.accept"

	PermissionOrganizationProfileRead   = "org.profile.read"
	PermissionOrganizationUnitsRead     = "org.units.read"
	PermissionOrganizationUnitsManage   = "org.units.manage"
	PermissionOrganizationMembersRead   = "org.members.read"
	PermissionOrganizationMembersManage = "org.members.manage"
	PermissionOrganizationRolesAssign   = "org.roles.assign"
	PermissionOrganizationOwnerTransfer = "org.owner.transfer"
	PermissionOrganizationQuotaAllocate = "org.quota.allocate"
)

type AuthorizationDecision struct {
	Allowed                          bool       `json:"allowed"`
	UserID                           uuid.UUID  `json:"user_id"`
	UserStatus                       string     `json:"user_status"`
	Permission                       string     `json:"permission"`
	PermissionScope                  string     `json:"permission_scope"`
	OrganizationID                   *uuid.UUID `json:"organization_id,omitempty"`
	ContextVersion                   int64      `json:"context_version"`
	AuthorizationVersion             int64      `json:"authorization_version"`
	PersonalAuthorizationVersion     int64      `json:"personal_authorization_version"`
	PlatformAuthorizationVersion     int64      `json:"platform_authorization_version"`
	OrganizationAuthorizationVersion int64      `json:"organization_authorization_version"`
}

type PermissionCheck struct {
	Issuer         string     `json:"issuer"`
	Subject        string     `json:"subject"`
	Permission     string     `json:"permission"`
	OrganizationID *uuid.UUID `json:"target_organization_id,omitempty"`
	UnitID         *uuid.UUID `json:"target_unit_id,omitempty"`
}

type authorizationRepository interface {
	Authorize(context.Context, identity.Principal, string, *uuid.UUID, *uuid.UUID) (AuthorizationDecision, error)
	RoleDefinitions(context.Context) ([]AdminRoleDefinition, error)
}

func (r *PostgresRepository) Authorize(ctx context.Context, principal identity.Principal, permission string, organizationID, unitID *uuid.UUID) (AuthorizationDecision, error) {
	permission = canonicalPermissionCode(permission)
	if permission == "" || (unitID != nil && organizationID == nil) {
		return AuthorizationDecision{}, ErrForbidden
	}
	var decision AuthorizationDecision
	var roleGranted bool
	decision.Permission = permission
	decision.OrganizationID = organizationID
	err := r.pool.QueryRow(ctx, `
WITH permission_definition AS (
    SELECT scope FROM permissions WHERE code=$3
)
SELECT u.id,u.status,COALESCE(active.context_version,1),COALESCE(pd.scope,''),
       COALESCE((SELECT version FROM authorization_versions WHERE user_id=u.id AND scope='PERSONAL' AND scope_id='00000000-0000-0000-0000-000000000000'::uuid),1),
       COALESCE((SELECT version FROM authorization_versions WHERE user_id=u.id AND scope='PLATFORM' AND scope_id='00000000-0000-0000-0000-000000000000'::uuid),1),
       COALESCE((SELECT version FROM authorization_versions WHERE user_id=u.id AND scope='ORGANIZATION' AND scope_id=$4),1),
       EXISTS(
         SELECT 1
         FROM role_assignments ra
         JOIN roles ro ON ro.code=ra.role_code
         JOIN role_permissions rp ON rp.role_code=ra.role_code AND rp.permission_code=$3
         LEFT JOIN memberships m ON m.user_id=ra.user_id AND m.organization_id=$4
         LEFT JOIN organizations o ON o.id=$4
         WHERE ra.user_id=u.id
	       AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now())
           AND (
             (pd.scope='PERSONAL' AND $4::uuid IS NULL AND $5::uuid IS NULL AND ro.scope='PERSONAL' AND ra.organization_id IS NULL AND ra.unit_id IS NULL)
             OR (pd.scope='PLATFORM' AND $4::uuid IS NULL AND $5::uuid IS NULL AND ro.scope='PLATFORM' AND ra.organization_id IS NULL AND ra.unit_id IS NULL)
             OR (pd.scope='ORGANIZATION' AND $4::uuid IS NOT NULL AND ra.organization_id=$4 AND m.status='ACTIVE' AND o.status='ACTIVE' AND (
               (ro.scope='ORGANIZATION' AND ra.unit_id IS NULL)
               OR (ro.scope='UNIT' AND $5::uuid IS NOT NULL AND EXISTS(
                 SELECT 1 FROM organization_unit_closure c
                 WHERE c.organization_id=$4 AND c.ancestor_id=ra.unit_id AND c.descendant_id=$5
               ))
             ))
           )
       )
FROM identities i
JOIN users u ON u.id=i.user_id
LEFT JOIN user_active_contexts active ON active.user_id=u.id
LEFT JOIN permission_definition pd ON true
WHERE i.issuer=$1 AND i.subject=$2`, principal.Issuer, principal.Subject, permission, organizationID, unitID).Scan(
		&decision.UserID, &decision.UserStatus, &decision.ContextVersion, &decision.PermissionScope,
		&decision.PersonalAuthorizationVersion, &decision.PlatformAuthorizationVersion,
		&decision.OrganizationAuthorizationVersion, &roleGranted,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return AuthorizationDecision{}, ErrForbidden
	}
	if err != nil {
		return AuthorizationDecision{}, err
	}
	decision.Allowed = roleGranted && permissionAllowedForUserStatus(decision.UserStatus, permission)
	switch decision.PermissionScope {
	case "PERSONAL":
		decision.AuthorizationVersion = decision.PersonalAuthorizationVersion
	case "PLATFORM":
		decision.AuthorizationVersion = decision.PlatformAuthorizationVersion
	case "ORGANIZATION", "UNIT":
		decision.AuthorizationVersion = decision.OrganizationAuthorizationVersion
	default:
		decision.AuthorizationVersion = 1
	}
	return decision, nil
}

func canonicalPermissionCode(permission string) string {
	switch strings.TrimSpace(permission) {
	case "model.invoke":
		return "org.model.invoke"
	case "organization.grant.reclaim_closed":
		return "org.grants.reclaim_closed"
	default:
		return strings.TrimSpace(permission)
	}
}

func permissionAllowedForUserStatus(status, permission string) bool {
	switch status {
	case "ACTIVE":
		return true
	case "PROVISIONING", "REPAIR_REQUIRED":
		switch permission {
		case PermissionPersonalSessionLogin, PermissionPersonalProfileRead, PermissionPersonalProfileUpdate, PermissionPersonalSecurityManage:
			return true
		}
	case "CLOSING":
		return permission == PermissionPersonalAccountClosureRead
	}
	return false
}

func (s *Service) contextVersionHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := identity.FromContext(c.Request.Context())
		if !ok {
			c.Next()
			return
		}
		me, err := s.repository.GetMe(c.Request.Context(), principal)
		if err == nil {
			c.Header("X-Context-Version", stringInt64(me.ActiveContext.ContextVersion))
			c.Header("X-Authorization-Version", stringInt64(me.ActiveContext.AuthorizationVersion))
			c.Header("X-Personal-Authorization-Version", stringInt64(me.ActiveContext.PersonalAuthorizationVersion))
			c.Header("X-Platform-Authorization-Version", stringInt64(me.ActiveContext.PlatformAuthorizationVersion))
			c.Header("X-Organization-Authorization-Version", stringInt64(me.ActiveContext.OrganizationAuthorizationVersion))
		}
		if rawOrganizationID := c.Param("organization_id"); rawOrganizationID != "" {
			if organizationID, parseErr := uuid.Parse(rawOrganizationID); parseErr == nil {
				if repository, available := s.repository.(authorizationRepository); available {
					decision, decisionErr := repository.Authorize(c.Request.Context(), principal, PermissionOrganizationProfileRead, &organizationID, nil)
					if decisionErr == nil {
						writeAuthorizationVersions(c.Writer.Header(), decision)
					}
				}
			}
		}
		c.Next()
	}
}

func (r *PostgresRepository) RoleDefinitions(ctx context.Context) ([]AdminRoleDefinition, error) {
	rows, err := r.pool.Query(ctx, `
SELECT r.code,r.scope,r.assignable,r.description,
       COALESCE(array_agg(rp.permission_code ORDER BY rp.permission_code) FILTER (WHERE rp.permission_code IS NOT NULL),'{}')
FROM roles r LEFT JOIN role_permissions rp ON rp.role_code=r.code
GROUP BY r.code,r.scope,r.assignable,r.description
ORDER BY r.scope,r.code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AdminRoleDefinition, 0)
	for rows.Next() {
		var item AdminRoleDefinition
		if err := rows.Scan(&item.Code, &item.Scope, &item.Assignable, &item.Description, &item.Permissions); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Service) requirePermission(permission string) gin.HandlerFunc {
	return func(c *gin.Context) {
		repository, ok := s.repository.(authorizationRepository)
		principal, authenticated := identity.FromContext(c.Request.Context())
		if !ok || !authenticated {
			api.WriteError(c.Writer, http.StatusForbidden, "PERMISSION_DENIED", "the requested permission is not granted", httpserver.RequestID(c.Request.Context()))
			c.Abort()
			return
		}
		decision, err := repository.Authorize(c.Request.Context(), principal, permission, nil, nil)
		if err != nil || !decision.Allowed {
			if auditor, auditOK := s.repository.(interface {
				RecordPermissionDenial(context.Context, identity.Principal, string, string) error
			}); auditOK {
				_ = auditor.RecordPermissionDenial(c.Request.Context(), principal, permission, c.Request.URL.Path)
			}
			api.WriteError(c.Writer, http.StatusForbidden, "PERMISSION_DENIED", "the requested permission is not granted", httpserver.RequestID(c.Request.Context()))
			c.Abort()
			return
		}
		writeAuthorizationVersions(c.Writer.Header(), decision)
		c.Next()
	}
}

func (r *PostgresRepository) RecordPermissionDenial(ctx context.Context, principal identity.Principal, permission, resourcePath string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return err
	}
	if err := appendAdminAuditResult(ctx, tx, actorID, principal, nil, "authorization.permission.denied", "http_route", resourcePath, "DENIED", map[string]any{"permission": strings.TrimSpace(permission)}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func writeAuthorizationVersions(header http.Header, decision AuthorizationDecision) {
	header.Set("X-Context-Version", strings.TrimSpace(stringInt64(decision.ContextVersion)))
	header.Set("X-Authorization-Version", strings.TrimSpace(stringInt64(decision.AuthorizationVersion)))
	header.Set("X-Personal-Authorization-Version", strings.TrimSpace(stringInt64(decision.PersonalAuthorizationVersion)))
	header.Set("X-Platform-Authorization-Version", strings.TrimSpace(stringInt64(decision.PlatformAuthorizationVersion)))
	header.Set("X-Organization-Authorization-Version", strings.TrimSpace(stringInt64(decision.OrganizationAuthorizationVersion)))
}

func stringInt64(value int64) string {
	if value < 1 {
		value = 1
	}
	return strconv.FormatInt(value, 10)
}

func (s *Service) checkPermission(w http.ResponseWriter, r *http.Request) {
	var request PermissionCheck
	if err := api.DecodeJSON(w, r, &request); err != nil {
		return
	}
	request.Issuer = strings.TrimSpace(request.Issuer)
	request.Subject = strings.TrimSpace(request.Subject)
	request.Permission = strings.TrimSpace(request.Permission)
	if request.Issuer == "" || request.Subject == "" || request.Permission == "" || (request.UnitID != nil && request.OrganizationID == nil) {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid permission check", httpserver.RequestID(r.Context()))
		return
	}
	repository, ok := s.repository.(authorizationRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "AUTHORIZATION_UNAVAILABLE", "authorization is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	decision, err := repository.Authorize(r.Context(), identity.Principal{Issuer: request.Issuer, Subject: request.Subject}, request.Permission, request.OrganizationID, request.UnitID)
	if err != nil && !errors.Is(err, ErrForbidden) {
		s.writeError(w, r, err)
		return
	}
	if errors.Is(err, ErrForbidden) {
		decision = AuthorizationDecision{Allowed: false, Permission: request.Permission, OrganizationID: request.OrganizationID, ContextVersion: 1, AuthorizationVersion: 1}
	}
	writeAuthorizationVersions(w.Header(), decision)
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, decision)
}

func requireOrganizationPermission(ctx context.Context, tx pgx.Tx, userID, organizationID uuid.UUID, unitID *uuid.UUID, permission string) error {
	permission = canonicalPermissionCode(permission)
	var allowed bool
	err := tx.QueryRow(ctx, `
SELECT EXISTS(
  SELECT 1
  FROM role_assignments ra
  JOIN memberships m ON m.user_id=ra.user_id AND m.organization_id=ra.organization_id AND m.status='ACTIVE'
  JOIN role_permissions rp ON rp.role_code=ra.role_code AND rp.permission_code=$4
  WHERE ra.user_id=$1 AND ra.organization_id=$2
	AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now())
    AND (ra.unit_id IS NULL OR ($3::uuid IS NOT NULL AND EXISTS(
      SELECT 1 FROM organization_unit_closure c
      WHERE c.organization_id=$2 AND c.ancestor_id=ra.unit_id AND c.descendant_id=$3
    )))
)`, userID, organizationID, unitID, permission).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrForbidden
	}
	return nil
}
