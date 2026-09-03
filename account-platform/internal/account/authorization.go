package account

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	platformauthorization "github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
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
	PermissionPlatformOrganizationsProfileManage = "platform.organizations.profile.manage"
	PermissionPlatformOrganizationsStatusManage  = "platform.organizations.status.manage"
	PermissionPlatformOrganizationsMembersManage = "platform.organizations.members.manage"
	PermissionPlatformOrganizationsUnitsManage   = "platform.organizations.units.manage"
	PermissionPlatformOrganizationsDelete        = "platform.organizations.delete"
	PermissionPlatformOrganizationsOwnerTransfer = "platform.organizations.owner.transfer"
	PermissionPlatformAuditRead                  = "platform.audit.read"
	PermissionPlatformAuditExport                = "platform.audit.export"
	PermissionPlatformAuditPIIExport             = "platform.audit.pii.export"
	PermissionPlatformSystemStatusRead           = "platform.system.status.read"
	PermissionPlatformRegistrationsRepair        = "platform.registrations.repair"
	PermissionPlatformProfileChangesRepair       = "platform.profile-changes.repair"
	PermissionPlatformTokenOwnerCandidatesRead   = "platform.tokens.owner-candidates.read"
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
	GrantedPermissions               []string   `json:"granted_permissions,omitempty"`
}

type PermissionCheck struct {
	Issuer         string     `json:"issuer"`
	Subject        string     `json:"subject"`
	Permission     string     `json:"permission"`
	AnyPermissions []string   `json:"any_permissions,omitempty"`
	AllPermissions []string   `json:"all_permissions,omitempty"`
	OrganizationID *uuid.UUID `json:"target_organization_id,omitempty"`
	UnitID         *uuid.UUID `json:"target_unit_id,omitempty"`
}

type authorizationRepository interface {
	Authorize(context.Context, identity.Principal, string, *uuid.UUID, *uuid.UUID) (AuthorizationDecision, error)
	RoleDefinitions(context.Context) ([]AdminRoleDefinition, error)
}

type compositeAuthorizationRepository interface {
	AuthorizeComposite(context.Context, identity.Principal, []string, []string) (AuthorizationDecision, error)
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

func canonicalPermissionSet(values []string) ([]string, error) {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = canonicalPermissionCode(value)
		if value == "" {
			return nil, ErrForbidden
		}
		if _, exists := seen[value]; !exists {
			seen[value] = struct{}{}
			result = append(result, value)
		}
	}
	if len(result) > 16 {
		return nil, ErrForbidden
	}
	return result, nil
}

// AuthorizeComposite evaluates a platform permission expression from one
// PostgreSQL statement, so grants and the authorization version are one
// indivisible snapshot. It intentionally supports only platform permissions.
func (r *PostgresRepository) AuthorizeComposite(ctx context.Context, principal identity.Principal, anyPermissions, allPermissions []string) (AuthorizationDecision, error) {
	anyPermissions, err := canonicalPermissionSet(anyPermissions)
	if err != nil || len(anyPermissions) == 0 {
		return AuthorizationDecision{}, ErrForbidden
	}
	allPermissions, err = canonicalPermissionSet(allPermissions)
	if err != nil {
		return AuthorizationDecision{}, ErrForbidden
	}
	requested, err := canonicalPermissionSet(append(append([]string{}, anyPermissions...), allPermissions...))
	if err != nil {
		return AuthorizationDecision{}, ErrForbidden
	}
	var decision AuthorizationDecision
	var allPlatform bool
	err = r.pool.QueryRow(ctx, `
WITH requested(permission) AS (SELECT DISTINCT unnest($3::text[])),
principal AS (
  SELECT account_user.id,account_user.status,COALESCE(active.context_version,1) context_version,
         COALESCE((SELECT version FROM authorization_versions WHERE user_id=account_user.id AND scope='PERSONAL' AND scope_id='00000000-0000-0000-0000-000000000000'::uuid),1) personal_version,
         COALESCE((SELECT version FROM authorization_versions WHERE user_id=account_user.id AND scope='PLATFORM' AND scope_id='00000000-0000-0000-0000-000000000000'::uuid),1) platform_version
  FROM identities identity_record
  JOIN users account_user ON account_user.id=identity_record.user_id
  LEFT JOIN user_active_contexts active ON active.user_id=account_user.id
  WHERE identity_record.issuer=$1 AND identity_record.subject=$2
), evaluated AS (
  SELECT requested.permission,permission.scope,
         EXISTS(
           SELECT 1 FROM role_assignments assignment
           JOIN roles role ON role.code=assignment.role_code AND role.scope='PLATFORM'
           JOIN role_permissions edge ON edge.role_code=assignment.role_code AND edge.permission_code=requested.permission
           WHERE assignment.user_id=principal.id AND assignment.organization_id IS NULL AND assignment.unit_id IS NULL
             AND assignment.revoked_at IS NULL AND assignment.starts_at<=transaction_timestamp()
             AND (assignment.expires_at IS NULL OR assignment.expires_at>transaction_timestamp())
         ) granted
  FROM principal CROSS JOIN requested LEFT JOIN permissions permission ON permission.code=requested.permission
)
SELECT principal.id,principal.status,principal.context_version,principal.personal_version,principal.platform_version,
       COALESCE(array_agg(evaluated.permission ORDER BY evaluated.permission) FILTER (WHERE evaluated.granted),'{}'::text[]),
       COALESCE(bool_and(evaluated.scope='PLATFORM'),false)
FROM principal CROSS JOIN evaluated
GROUP BY principal.id,principal.status,principal.context_version,principal.personal_version,principal.platform_version`, principal.Issuer, principal.Subject, requested).Scan(
		&decision.UserID, &decision.UserStatus, &decision.ContextVersion, &decision.PersonalAuthorizationVersion,
		&decision.PlatformAuthorizationVersion, &decision.GrantedPermissions, &allPlatform,
	)
	if errors.Is(err, pgx.ErrNoRows) || !allPlatform {
		return AuthorizationDecision{}, ErrForbidden
	}
	if err != nil {
		return AuthorizationDecision{}, err
	}
	decision.PermissionScope = "PLATFORM"
	decision.AuthorizationVersion = decision.PlatformAuthorizationVersion
	granted := make(map[string]struct{}, len(decision.GrantedPermissions))
	for _, permission := range decision.GrantedPermissions {
		granted[permission] = struct{}{}
	}
	allGranted := true
	for _, permission := range allPermissions {
		_, ok := granted[permission]
		allGranted = allGranted && ok
	}
	anyGranted := false
	for _, permission := range anyPermissions {
		_, ok := granted[permission]
		anyGranted = anyGranted || ok
	}
	statusPermission := anyPermissions[0]
	if len(allPermissions) > 0 {
		statusPermission = allPermissions[0]
	}
	decision.Allowed = permissionAllowedForUserStatus(decision.UserStatus, statusPermission) && allGranted && anyGranted
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

func (s *Service) requireAnyPermission(permissions ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		repository, ok := s.repository.(authorizationRepository)
		principal, authenticated := identity.FromContext(c.Request.Context())
		if !ok || !authenticated {
			api.WriteError(c.Writer, http.StatusForbidden, "PERMISSION_DENIED", "none of the requested permissions is granted", httpserver.RequestID(c.Request.Context()))
			c.Abort()
			return
		}
		granted := make([]string, 0, len(permissions))
		var lastDecision AuthorizationDecision
		for _, permission := range permissions {
			decision, err := repository.Authorize(c.Request.Context(), principal, permission, nil, nil)
			if err != nil {
				api.WriteError(c.Writer, http.StatusServiceUnavailable, "AUTHORIZATION_UNAVAILABLE", "authorization is temporarily unavailable", httpserver.RequestID(c.Request.Context()))
				c.Abort()
				return
			}
			lastDecision = decision
			if decision.Allowed {
				granted = append(granted, permission)
			}
		}
		if len(granted) == 0 {
			api.WriteError(c.Writer, http.StatusForbidden, "PERMISSION_DENIED", "none of the requested permissions is granted", httpserver.RequestID(c.Request.Context()))
			c.Abort()
			return
		}
		writeAuthorizationVersions(c.Writer.Header(), lastDecision)
		c.Request = c.Request.WithContext(platformauthorization.WithGrantedPermissions(c.Request.Context(), granted...))
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
	composite := len(request.AnyPermissions) > 0 || len(request.AllPermissions) > 0
	if request.Issuer == "" || request.Subject == "" || (request.Permission == "") == !composite || (request.UnitID != nil && request.OrganizationID == nil) || (composite && (request.OrganizationID != nil || request.UnitID != nil)) {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid permission check", httpserver.RequestID(r.Context()))
		return
	}
	repository, ok := s.repository.(authorizationRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "AUTHORIZATION_UNAVAILABLE", "authorization is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	var decision AuthorizationDecision
	var err error
	if composite {
		compositeRepository, available := s.repository.(compositeAuthorizationRepository)
		if !available {
			api.WriteError(w, http.StatusServiceUnavailable, "AUTHORIZATION_UNAVAILABLE", "authorization is unavailable", httpserver.RequestID(r.Context()))
			return
		}
		decision, err = compositeRepository.AuthorizeComposite(r.Context(), identity.Principal{Issuer: request.Issuer, Subject: request.Subject}, request.AnyPermissions, request.AllPermissions)
	} else {
		decision, err = repository.Authorize(r.Context(), identity.Principal{Issuer: request.Issuer, Subject: request.Subject}, request.Permission, request.OrganizationID, request.UnitID)
	}
	if err != nil && !errors.Is(err, ErrForbidden) {
		s.writeError(w, r, err)
		return
	}
	if errors.Is(err, ErrForbidden) {
		decision = AuthorizationDecision{Allowed: false, Permission: request.Permission, OrganizationID: request.OrganizationID}
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
  JOIN organizations o ON o.id=ra.organization_id AND o.status='ACTIVE'
  JOIN role_permissions rp ON rp.role_code=ra.role_code AND rp.permission_code=$4
  WHERE ra.user_id=$1 AND ra.organization_id=$2
	AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now())
    AND ($3::uuid IS NULL OR EXISTS(
      SELECT 1 FROM organization_units target
      WHERE target.organization_id=$2 AND target.id=$3 AND target.status='ACTIVE'
    ))
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

func requireOrganizationMemberPermission(ctx context.Context, tx pgx.Tx, userID, organizationID, targetUserID uuid.UUID, permission string) error {
	permission = canonicalPermissionCode(permission)
	var allowed bool
	err := tx.QueryRow(ctx, `
WITH target_membership AS (
  SELECT membership.id
  FROM memberships membership
  JOIN organizations organization ON organization.id=membership.organization_id AND organization.status='ACTIVE'
  WHERE membership.organization_id=$2 AND membership.user_id=$3 AND membership.status='ACTIVE'
), authorized_roots AS (
  SELECT actor_role.unit_id
  FROM role_assignments actor_role
  JOIN memberships actor_membership ON actor_membership.user_id=actor_role.user_id AND actor_membership.organization_id=actor_role.organization_id AND actor_membership.status='ACTIVE'
  JOIN role_permissions permission ON permission.role_code=actor_role.role_code AND permission.permission_code=$4
  WHERE actor_role.user_id=$1 AND actor_role.organization_id=$2
    AND actor_role.revoked_at IS NULL AND actor_role.starts_at<=now() AND (actor_role.expires_at IS NULL OR actor_role.expires_at>now())
)
SELECT EXISTS(SELECT 1 FROM target_membership) AND (
  EXISTS(SELECT 1 FROM authorized_roots WHERE unit_id IS NULL)
  OR (
    EXISTS(SELECT 1 FROM authorized_roots WHERE unit_id IS NOT NULL)
    AND EXISTS(SELECT 1 FROM membership_units target_unit JOIN target_membership target ON target.id=target_unit.membership_id)
    AND NOT EXISTS(
      SELECT 1
      FROM membership_units target_unit
      JOIN target_membership target ON target.id=target_unit.membership_id
      WHERE NOT EXISTS(
        SELECT 1
        FROM authorized_roots root
        JOIN organization_unit_closure closure ON closure.organization_id=$2 AND closure.ancestor_id=root.unit_id AND closure.descendant_id=target_unit.unit_id
        WHERE root.unit_id IS NOT NULL
      )
    )
  )
)`, userID, organizationID, targetUserID, permission).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrForbidden
	}
	return nil
}
