package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresRepository struct {
	pool                  *pgxpool.Pool
	now                   func() time.Time
	walletPolicyVersion   string
	walletProvisioningSLA time.Duration
}

func NewPostgresRepository(ctx context.Context, databaseURL string) (*PostgresRepository, error) {
	configuration, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse account database URL: %w", err)
	}
	configuration.MaxConns = 20
	configuration.MinConns = 1
	configuration.MaxConnLifetime = 30 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		return nil, fmt.Errorf("open account database: %w", err)
	}
	repository := &PostgresRepository{
		pool: pool, now: func() time.Time { return time.Now().UTC() },
		walletPolicyVersion: walletProvisioningPolicyVersion, walletProvisioningSLA: walletProvisioningSLA,
	}
	if err := repository.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return repository, nil
}

func (r *PostgresRepository) ConfigureWalletProvisioning(policyVersion string, sla time.Duration) error {
	policyVersion = strings.TrimSpace(policyVersion)
	if policyVersion == "" || sla < time.Minute || sla > 24*time.Hour {
		return errors.New("wallet provisioning policy requires a version and a 1 minute to 24 hour SLA")
	}
	r.walletPolicyVersion, r.walletProvisioningSLA = policyVersion, sla
	return nil
}

func (r *PostgresRepository) Ping(ctx context.Context) error { return r.pool.Ping(ctx) }

func (r *PostgresRepository) userIDByPrincipal(ctx context.Context, principal identity.Principal) (uuid.UUID, error) {
	var userID uuid.UUID
	err := r.pool.QueryRow(ctx, `SELECT u.id FROM identities i JOIN users u ON u.id=i.user_id AND u.status='ACTIVE' WHERE i.issuer=$1 AND i.subject=$2`, principal.Issuer, principal.Subject).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrForbidden
	}
	return userID, err
}
func (r *PostgresRepository) Close() {
	if r.pool != nil {
		r.pool.Close()
	}
}

func (r *PostgresRepository) Bootstrap(ctx context.Context, principal identity.Principal) (Me, bool, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Me{}, false, err
	}
	defer tx.Rollback(ctx)

	lockKey := fmt.Sprintf("%d:%s%s", len(principal.Issuer), principal.Issuer, principal.Subject)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, lockKey); err != nil {
		return Me{}, false, err
	}
	user, err := loadUserByIdentity(ctx, tx, principal)
	created := false
	if errors.Is(err, pgx.ErrNoRows) {
		created = true
		now := r.now()
		user.UserID = uuid.Must(uuid.NewV7())
		user.Status = "PROVISIONING"
		user.ActiveContext = Context{Type: "PERSONAL", MembershipVersion: 1, ContextVersion: 1, AuthorizationVersion: 1}
		if username := strings.TrimSpace(principal.Username); username != "" {
			user.Username = &username
		}
		if principal.DisplayName != "" {
			name := principal.DisplayName
			user.DisplayName = &name
		}
		var userCode string
		userCode, err = insertUserWithGeneratedCode(ctx, tx, func(database pgx.Tx, candidate string) error {
			_, insertErr := database.Exec(ctx, `
INSERT INTO users(id, user_code, status, username, display_name, email, created_at, updated_at)
VALUES ($1, $2, 'PROVISIONING', NULLIF($3, ''), NULLIF($4, ''), NULLIF($5, ''), $6, $6)`,
				user.UserID, candidate, strings.TrimSpace(principal.Username), principal.DisplayName, normalizeEmail(principal.Email), now)
			return insertErr
		})
		user.UserCode = FormatUserCode(userCode)
		if err == nil {
			_, err = tx.Exec(ctx, `
INSERT INTO identities(issuer, subject, user_id, created_at, last_seen_at)
VALUES ($1, $2, $3, $4, $4)`, principal.Issuer, principal.Subject, user.UserID, now)
		}
	} else if err != nil {
		return Me{}, false, err
	}
	if err != nil {
		return Me{}, false, err
	}
	if personalRoleEligibleStatus(user.Status) {
		if err = ensureDefaultPersonalRole(ctx, tx, user.UserID, r.now()); err != nil {
			return Me{}, false, err
		}
	}
	if !created && user.Username == nil && strings.TrimSpace(principal.Username) != "" {
		username := strings.TrimSpace(principal.Username)
		if _, err = tx.Exec(ctx, `UPDATE users SET username=$2,updated_at=$3 WHERE id=$1 AND username IS NULL`, user.UserID, username, r.now()); err != nil {
			return Me{}, false, err
		}
		user.Username = &username
	}
	if user.Status == "BOOTSTRAP_PENDING" {
		return Me{}, false, ErrBootstrapPending
	}

	now := r.now()
	_, err = tx.Exec(ctx, `
INSERT INTO user_active_contexts(user_id,context_type,organization_id,context_version,updated_at)
VALUES($1,'PERSONAL',NULL,1,$2) ON CONFLICT(user_id) DO NOTHING`, user.UserID, now)
	if err != nil {
		return Me{}, false, err
	}
	_, err = tx.Exec(ctx, `
INSERT INTO user_sessions(issuer, session_id, user_id, active_context_type, membership_version, last_seen_at, created_at)
VALUES ($1, $2, $3, 'PERSONAL', $4, $5, $5)
ON CONFLICT (issuer, session_id) DO UPDATE
SET last_seen_at = EXCLUDED.last_seen_at
WHERE user_sessions.user_id = EXCLUDED.user_id`,
		principal.Issuer, principal.SessionID, user.UserID, user.ActiveContext.MembershipVersion, now)
	if err != nil {
		return Me{}, false, err
	}
	if err = tx.QueryRow(ctx, `
SELECT context.context_type,context.organization_id,context.context_version,u.membership_version,
       COALESCE((SELECT version FROM authorization_versions WHERE user_id=u.id
         AND scope=CASE WHEN context.organization_id IS NULL THEN 'PERSONAL' ELSE 'ORGANIZATION' END
         AND scope_id=COALESCE(context.organization_id,'00000000-0000-0000-0000-000000000000'::uuid)),1),
       COALESCE((SELECT version FROM authorization_versions WHERE user_id=u.id AND scope='PERSONAL' AND scope_id='00000000-0000-0000-0000-000000000000'::uuid),1),
       COALESCE((SELECT version FROM authorization_versions WHERE user_id=u.id AND scope='PLATFORM' AND scope_id='00000000-0000-0000-0000-000000000000'::uuid),1),
       COALESCE((SELECT version FROM authorization_versions WHERE user_id=u.id AND scope='ORGANIZATION' AND scope_id=context.organization_id),1),
       COALESCE((
         SELECT array_agg(DISTINCT rp.permission_code ORDER BY rp.permission_code)
         FROM role_assignments ra JOIN roles ro ON ro.code=ra.role_code
         JOIN role_permissions rp ON rp.role_code=ra.role_code
         WHERE ra.user_id=u.id AND ra.organization_id IS NOT DISTINCT FROM context.organization_id
           AND ((context.organization_id IS NULL AND ro.scope='PERSONAL') OR (context.organization_id IS NOT NULL AND ro.scope='ORGANIZATION'))
           AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now())
       ),'{}'),
       COALESCE((
         SELECT array_agg(DISTINCT rp.permission_code ORDER BY rp.permission_code)
         FROM role_assignments ra JOIN roles ro ON ro.code=ra.role_code
         JOIN role_permissions rp ON rp.role_code=ra.role_code
         WHERE ra.user_id=u.id AND ra.organization_id IS NULL AND ra.unit_id IS NULL AND ro.scope='PERSONAL'
           AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now())
       ),'{}'),
       COALESCE((
         SELECT array_agg(DISTINCT rp.permission_code ORDER BY rp.permission_code)
         FROM role_assignments ra JOIN roles ro ON ro.code=ra.role_code
         JOIN role_permissions rp ON rp.role_code=ra.role_code
         WHERE ra.user_id=u.id AND ra.organization_id IS NULL AND ra.unit_id IS NULL AND ro.scope='PLATFORM'
           AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now())
       ),'{}')
FROM users u JOIN user_active_contexts context ON context.user_id=u.id WHERE u.id=$1`, user.UserID).Scan(
		&user.ActiveContext.Type, &user.ActiveContext.OrganizationID, &user.ActiveContext.ContextVersion,
		&user.ActiveContext.MembershipVersion, &user.ActiveContext.AuthorizationVersion,
		&user.ActiveContext.PersonalAuthorizationVersion, &user.ActiveContext.PlatformAuthorizationVersion,
		&user.ActiveContext.OrganizationAuthorizationVersion, &user.ActiveContext.Permissions,
		&user.PersonalPermissions, &user.PlatformPermissions,
	); err != nil {
		return Me{}, false, err
	}
	_, err = tx.Exec(ctx, `UPDATE identities SET last_seen_at = $3 WHERE issuer = $1 AND subject = $2`, principal.Issuer, principal.Subject, now)
	if err != nil {
		return Me{}, false, err
	}
	if created {
		if err := appendAudit(ctx, tx, user.UserID, principal.SessionID, nil, "account.bootstrap", "user", user.UserID.String(), "SUCCEEDED", map[string]any{"status": "PROVISIONING"}); err != nil {
			return Me{}, false, err
		}
	}
	if user.Status == "PROVISIONING" {
		if err := r.ensureWalletProvisioningJob(ctx, tx, user.UserID, nil, now); err != nil {
			return Me{}, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Me{}, false, err
	}
	filterMePermissions(&user)
	return user, created, nil
}

func (r *PostgresRepository) ensureWalletProvisioningJob(ctx context.Context, tx pgx.Tx, userID uuid.UUID, operationID *uuid.UUID, now time.Time) error {
	command, err := tx.Exec(ctx, `
INSERT INTO wallet_provisioning_jobs(user_id,registration_operation_id,status,policy_version,sla_expires_at,next_attempt_at,created_at,updated_at)
VALUES($1,$2,'PENDING',$3,$4,$5,$5,$5) ON CONFLICT (user_id) DO NOTHING`, userID, operationID, r.walletPolicyVersion, now.Add(r.walletProvisioningSLA), now)
	if err != nil || command.RowsAffected() == 0 {
		return err
	}
	payload := map[string]any{"user_id": userID, "policy_version": r.walletPolicyVersion}
	if operationID != nil {
		payload["registration_operation_id"] = *operationID
	}
	return appendOutbox(ctx, tx, "account.wallet.provisioning.requested.v1", userID, 1, payload)
}

func (r *PostgresRepository) GetMe(ctx context.Context, principal identity.Principal) (Me, error) {
	var me Me
	var organizationID *uuid.UUID
	err := r.pool.QueryRow(ctx, `
SELECT u.id, u.user_code, u.status, u.username, u.display_name, u.mobile_e164, u.mobile_verified_at IS NOT NULL,
       context.context_type, context.organization_id,
       u.membership_version,context.context_version,
       COALESCE((SELECT av.version FROM authorization_versions av WHERE av.user_id=u.id AND av.scope=CASE WHEN context.organization_id IS NULL THEN 'PERSONAL' ELSE 'ORGANIZATION' END AND av.scope_id=COALESCE(context.organization_id,'00000000-0000-0000-0000-000000000000'::uuid)),1),
       COALESCE((SELECT version FROM authorization_versions WHERE user_id=u.id AND scope='PERSONAL' AND scope_id='00000000-0000-0000-0000-000000000000'::uuid),1),
       COALESCE((SELECT version FROM authorization_versions WHERE user_id=u.id AND scope='PLATFORM' AND scope_id='00000000-0000-0000-0000-000000000000'::uuid),1),
       COALESCE((SELECT version FROM authorization_versions WHERE user_id=u.id AND scope='ORGANIZATION' AND scope_id=context.organization_id),1),
       COALESCE((
         SELECT array_agg(DISTINCT rp.permission_code ORDER BY rp.permission_code)
         FROM role_assignments ra JOIN roles ro ON ro.code=ra.role_code
         JOIN role_permissions rp ON rp.role_code=ra.role_code
         WHERE ra.user_id=u.id AND ra.organization_id IS NOT DISTINCT FROM context.organization_id
           AND ((context.organization_id IS NULL AND ro.scope='PERSONAL') OR (context.organization_id IS NOT NULL AND ro.scope='ORGANIZATION'))
           AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now())
       ),'{}'),
       COALESCE((
         SELECT array_agg(DISTINCT rp.permission_code ORDER BY rp.permission_code)
         FROM role_assignments ra JOIN roles ro ON ro.code=ra.role_code
         JOIN role_permissions rp ON rp.role_code=ra.role_code
         WHERE ra.user_id=u.id AND ra.organization_id IS NULL AND ra.unit_id IS NULL AND ro.scope='PERSONAL'
           AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now())
       ),'{}'),
       COALESCE((
         SELECT array_agg(DISTINCT rp.permission_code ORDER BY rp.permission_code)
         FROM role_assignments ra JOIN roles ro ON ro.code=ra.role_code
         JOIN role_permissions rp ON rp.role_code=ra.role_code
         WHERE ra.user_id=u.id AND ra.organization_id IS NULL AND ra.unit_id IS NULL AND ro.scope='PLATFORM'
           AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now())
       ),'{}')
FROM identities i
JOIN users u ON u.id = i.user_id
JOIN user_sessions s ON s.user_id = u.id AND s.issuer = i.issuer AND s.session_id = $3
JOIN user_active_contexts context ON context.user_id=u.id
WHERE i.issuer = $1 AND i.subject = $2`, principal.Issuer, principal.Subject, principal.SessionID).Scan(
		&me.UserID, &me.UserCode, &me.Status, &me.Username, &me.DisplayName, &me.Mobile, &me.MobileVerified,
		&me.ActiveContext.Type, &organizationID, &me.ActiveContext.MembershipVersion,
		&me.ActiveContext.ContextVersion, &me.ActiveContext.AuthorizationVersion,
		&me.ActiveContext.PersonalAuthorizationVersion, &me.ActiveContext.PlatformAuthorizationVersion,
		&me.ActiveContext.OrganizationAuthorizationVersion, &me.ActiveContext.Permissions,
		&me.PersonalPermissions, &me.PlatformPermissions,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Me{}, ErrNotFound
	}
	me.ActiveContext.OrganizationID = organizationID
	me.UserCode = FormatUserCode(me.UserCode)
	filterMePermissions(&me)
	return me, err
}

func (r *PostgresRepository) ResolveAuthorizationContext(ctx context.Context, principal identity.Principal) (AuthorizationContext, error) {
	var resolved AuthorizationContext
	err := r.pool.QueryRow(ctx, `SELECT u.id,s.session_id,active.context_type,u.membership_version,active.context_version,
 COALESCE((SELECT array_agg(DISTINCT ra.role_code ORDER BY ra.role_code) FROM role_assignments ra JOIN roles ro ON ro.code=ra.role_code WHERE ra.user_id=u.id AND ra.organization_id IS NULL AND ra.unit_id IS NULL AND ro.scope='PERSONAL' AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now())),'{}'),
 COALESCE((SELECT array_agg(DISTINCT rp.permission_code ORDER BY rp.permission_code) FROM role_assignments ra JOIN roles ro ON ro.code=ra.role_code JOIN role_permissions rp ON rp.role_code=ra.role_code WHERE ra.user_id=u.id AND ra.organization_id IS NULL AND ra.unit_id IS NULL AND ro.scope='PERSONAL' AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now())),'{}'),
 COALESCE((SELECT array_agg(DISTINCT ra.role_code ORDER BY ra.role_code) FROM role_assignments ra JOIN roles ro ON ro.code=ra.role_code WHERE ra.user_id=u.id AND ra.organization_id IS NULL AND ra.unit_id IS NULL AND ro.scope='PLATFORM' AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now())),'{}'),
 COALESCE((SELECT array_agg(DISTINCT rp.permission_code ORDER BY rp.permission_code) FROM role_assignments ra JOIN roles ro ON ro.code=ra.role_code JOIN role_permissions rp ON rp.role_code=ra.role_code WHERE ra.user_id=u.id AND ra.organization_id IS NULL AND ra.unit_id IS NULL AND ro.scope='PLATFORM' AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now())),'{}'),
 COALESCE((SELECT version FROM authorization_versions WHERE user_id=u.id AND scope='PERSONAL' AND scope_id='00000000-0000-0000-0000-000000000000'::uuid),1),
 COALESCE((SELECT version FROM authorization_versions WHERE user_id=u.id AND scope='PLATFORM' AND scope_id='00000000-0000-0000-0000-000000000000'::uuid),1),u.mobile_e164,u.mobile_verified_at IS NOT NULL
FROM identities i JOIN users u ON u.id=i.user_id AND u.status='ACTIVE'
JOIN user_sessions s ON s.user_id=u.id AND s.issuer=i.issuer AND s.session_id=$3
JOIN user_active_contexts active ON active.user_id=u.id AND active.context_type='PERSONAL' AND active.organization_id IS NULL
WHERE i.issuer=$1 AND i.subject=$2`, principal.Issuer, principal.Subject, principal.SessionID).Scan(
		&resolved.UserID, &resolved.SessionID, &resolved.ContextType, &resolved.MembershipVersion, &resolved.ContextVersion,
		&resolved.PersonalRoles, &resolved.PersonalPermissions, &resolved.PlatformRoles, &resolved.PlatformPermissions,
		&resolved.PersonalAuthorizationVersion, &resolved.PlatformAuthorizationVersion, &resolved.Mobile, &resolved.MobileVerified)
	resolved.AuthorizationVersion = resolved.PersonalAuthorizationVersion
	if errors.Is(err, pgx.ErrNoRows) {
		var status string
		statusErr := r.pool.QueryRow(ctx, `SELECT u.status FROM identities i JOIN users u ON u.id=i.user_id WHERE i.issuer=$1 AND i.subject=$2`, principal.Issuer, principal.Subject).Scan(&status)
		if statusErr == nil && status == "PROVISIONING" {
			return AuthorizationContext{}, ErrAccountProvisioning
		}
		if statusErr == nil && status == "REPAIR_REQUIRED" {
			return AuthorizationContext{}, ErrAccountRepairRequired
		}
		if statusErr == nil && status == "CLOSING" {
			return AuthorizationContext{}, ErrAccountClosing
		}
		return AuthorizationContext{}, ErrNotFound
	}
	if err != nil {
		return AuthorizationContext{}, err
	}
	resolved.PersonalPermissions = filterF01Permissions(resolved.PersonalPermissions)
	resolved.PlatformPermissions = filterF01Permissions(resolved.PlatformPermissions)
	normalizeActiveScopeAuthorization(&resolved)
	if resolved.ContextType == "ORGANIZATION" && resolved.OrganizationID != nil && len(resolved.Roles) == 0 {
		return AuthorizationContext{}, ErrForbidden
	}
	if resolved.UnitIDs == nil {
		resolved.UnitIDs = []uuid.UUID{}
	}
	if resolved.AncestorUnitIDs == nil {
		resolved.AncestorUnitIDs = []uuid.UUID{}
	}
	if resolved.Roles == nil {
		resolved.Roles = []string{}
	}
	if resolved.AuthorizationVersion < 1 {
		resolved.AuthorizationVersion = 1
	}
	if resolved.PersonalAuthorizationVersion < 1 {
		resolved.PersonalAuthorizationVersion = 1
	}
	if resolved.PlatformAuthorizationVersion < 1 {
		resolved.PlatformAuthorizationVersion = 1
	}
	if resolved.OrganizationAuthorizationVersion < 1 {
		resolved.OrganizationAuthorizationVersion = 1
	}
	if resolved.Permissions == nil {
		resolved.Permissions = []string{}
	}
	if resolved.UnitAuthorizationBindings == nil {
		resolved.UnitAuthorizationBindings = []UnitAuthorizationBinding{}
	}
	if resolved.PlatformRoles == nil {
		resolved.PlatformRoles = []string{}
	}
	if resolved.PlatformPermissions == nil {
		resolved.PlatformPermissions = []string{}
	}
	if resolved.PersonalRoles == nil {
		resolved.PersonalRoles = []string{}
	}
	if resolved.PersonalPermissions == nil {
		resolved.PersonalPermissions = []string{}
	}
	return resolved, nil
}

// Permissions and Roles are the authorization snapshot for the active billing
// and model-usage context. The three-scope response also exposes personal and
// platform grants separately for management UIs. In a PERSONAL context there
// is no organization_id, so the organization-scoped SQL projection above is
// intentionally empty and must be replaced by the personal projection.
func normalizeActiveScopeAuthorization(resolved *AuthorizationContext) {
	if resolved == nil || resolved.ContextType != "PERSONAL" {
		return
	}
	resolved.Roles = append([]string(nil), resolved.PersonalRoles...)
	resolved.Permissions = append([]string(nil), resolved.PersonalPermissions...)
}

func loadUserByIdentity(ctx context.Context, tx pgx.Tx, principal identity.Principal) (Me, error) {
	var me Me
	err := tx.QueryRow(ctx, `SELECT u.id,u.user_code,u.status,u.username,u.display_name,u.mobile_e164,u.mobile_verified_at IS NOT NULL,u.membership_version FROM identities i JOIN users u ON u.id=i.user_id WHERE i.issuer=$1 AND i.subject=$2`, principal.Issuer, principal.Subject).Scan(&me.UserID, &me.UserCode, &me.Status, &me.Username, &me.DisplayName, &me.Mobile, &me.MobileVerified, &me.ActiveContext.MembershipVersion)
	me.ActiveContext.Type = "PERSONAL"
	me.UserCode = FormatUserCode(me.UserCode)
	me.ActiveContext.ContextVersion = me.ActiveContext.MembershipVersion
	me.ActiveContext.AuthorizationVersion = 1
	me.ActiveContext.PersonalAuthorizationVersion = 1
	me.ActiveContext.PlatformAuthorizationVersion = 1
	me.ActiveContext.OrganizationAuthorizationVersion = 1
	filterMePermissions(&me)
	return me, err
}

func activeUserID(ctx context.Context, tx pgx.Tx, principal identity.Principal) (uuid.UUID, error) {
	var userID uuid.UUID
	err := tx.QueryRow(ctx, `SELECT u.id FROM identities i JOIN users u ON u.id = i.user_id WHERE i.issuer = $1 AND i.subject = $2 AND u.status = 'ACTIVE'`, principal.Issuer, principal.Subject).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrForbidden
	}
	return userID, err
}

// activeOrClosingUserID is intentionally used only by the ownership handoff
// repair action. A closing account remains unable to create any other business
// or authorization fact.
func normalizeEmail(value string) string { return strings.ToLower(strings.TrimSpace(value)) }

func appendAudit(ctx context.Context, tx pgx.Tx, actor uuid.UUID, sessionID string, organizationID *uuid.UUID, action, resourceType, resourceID, result string, summary map[string]any) error {
	if summary == nil {
		summary = map[string]any{}
	}
	body, err := json.Marshal(summary)
	if err != nil {
		return err
	}
	requestID := httpserver.RequestID(ctx)
	if requestID == "" {
		requestID = "internal"
	}
	_, err = tx.Exec(ctx, `
INSERT INTO audit_logs(id, actor_user_id, session_id, active_organization_id, action, resource_type, resource_id, result, request_id, trace_id, change_summary, occurred_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9, $10, now())`, uuid.Must(uuid.NewV7()), actor, sessionID, organizationID, action, resourceType, resourceID, result, requestID, body)
	return err
}

func appendOutbox(ctx context.Context, tx pgx.Tx, eventType string, aggregateID uuid.UUID, aggregateVersion int64, payload map[string]any) error {
	return events.AppendOutbox(ctx, tx, eventType, aggregateID, aggregateVersion, payload)
}

func mapConstraintError(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "violates check constraint") || strings.Contains(err.Error(), "violates foreign key") {
		return ErrConflict
	}
	return err
}
