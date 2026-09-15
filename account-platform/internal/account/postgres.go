package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/billinggovernance"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/tokengovernance"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresRepository struct {
	pool               *pgxpool.Pool
	approvalWriterPool *pgxpool.Pool
	// approvalWriterCommit is an instance-local fault seam used only by package
	// integration tests. Production constructors leave it nil, so every commit
	// goes directly to pgx.Tx.Commit; there is no runtime setting or fallback.
	approvalWriterCommit   func(context.Context, pgx.Tx) error
	now                    func() time.Time
	walletPolicyVersion    string
	walletProvisioningSLA  time.Duration
	commerceProviderID     string
	commerceAdapterVersion string
	approvalSnapshots      ApprovalSnapshotSource
	// tokenApprovalCreateGate is nil in every production constructor. APT-138
	// owns the eventual activation source; the package integration gate injects
	// an in-memory decision only to exercise the otherwise dormant transaction
	// chain without changing PRE_ACTIVATION behavior.
	tokenApprovalCreateGate     func(string) bool
	tokenGovernanceActivation   tokengovernance.Reader
	billingGovernanceActivation billinggovernance.Reader
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
		commerceProviderID: defaultCommerceProviderID, commerceAdapterVersion: defaultCommerceAdapterVersion,
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
func (r *PostgresRepository) Close() {
	if r.approvalWriterPool != nil && r.approvalWriterPool != r.pool {
		r.approvalWriterPool.Close()
	}
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
	user, err := loadUserByIdentity(ctx, tx, principal, r.commerceProviderID)
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
	return user, created, nil
}

func (r *PostgresRepository) ensureWalletProvisioningJob(ctx context.Context, tx pgx.Tx, userID uuid.UUID, operationID *uuid.UUID, now time.Time) error {
	command, err := tx.Exec(ctx, `
INSERT INTO wallet_provisioning_jobs(user_id,registration_operation_id,status,policy_version,sla_expires_at,next_attempt_at,created_at,updated_at)
VALUES($1,$2,'PENDING',$3,$4,$5,$5,$5) ON CONFLICT (user_id) DO NOTHING`, userID, operationID, r.walletPolicyVersion, now.Add(r.walletProvisioningSLA), now)
	if err != nil || command.RowsAffected() == 0 {
		return err
	}
	return appendOutbox(ctx, tx, "account.wallet.provisioning.requested.v1", userID, 1, map[string]any{"user_id": userID, "policy_version": r.walletPolicyVersion})
}

func (r *PostgresRepository) ActivateUser(ctx context.Context, userID uuid.UUID) error {
	command, err := r.pool.Exec(ctx, `
UPDATE users SET status = 'ACTIVE', version = version + 1, updated_at = $2
WHERE id = $1 AND status = 'PROVISIONING'`, userID, r.now())
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		var status string
		if err := r.pool.QueryRow(ctx, `SELECT status FROM users WHERE id = $1`, userID).Scan(&status); err != nil {
			return err
		}
		if status != "ACTIVE" {
			return ErrConflict
		}
	}
	return nil
}

func (r *PostgresRepository) GetMe(ctx context.Context, principal identity.Principal) (Me, error) {
	var me Me
	var organizationID *uuid.UUID
	err := r.pool.QueryRow(ctx, `
SELECT u.id, u.user_code, u.status, u.username, u.display_name, u.mobile_e164, u.mobile_verified_at IS NOT NULL,
       COALESCE(cm.status,'NOT_CONFIGURED'), context.context_type, context.organization_id,
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
LEFT JOIN commerce_customer_mappings cm ON cm.subject_type='USER' AND cm.subject_id=u.id AND cm.provider_id=$4
WHERE i.issuer = $1 AND i.subject = $2`, principal.Issuer, principal.Subject, principal.SessionID, r.commerceProviderID).Scan(
		&me.UserID, &me.UserCode, &me.Status, &me.Username, &me.DisplayName, &me.Mobile, &me.MobileVerified,
		&me.CommerceCustomerStatus, &me.ActiveContext.Type, &organizationID, &me.ActiveContext.MembershipVersion,
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
	return me, err
}

func (r *PostgresRepository) ResolveAuthorizationContext(ctx context.Context, principal identity.Principal) (AuthorizationContext, error) {
	var resolved AuthorizationContext
	err := r.pool.QueryRow(ctx, `
SELECT u.id,s.session_id,active.context_type,active.organization_id,u.membership_version,active.context_version,
       COALESCE(array_agg(DISTINCT mu.unit_id) FILTER (WHERE mu.unit_id IS NOT NULL),'{}'),
       COALESCE(array_agg(DISTINCT c.ancestor_id) FILTER (WHERE c.ancestor_id IS NOT NULL),'{}'),
       COALESCE((SELECT array_agg(DISTINCT ra.role_code ORDER BY ra.role_code) FROM role_assignments ra JOIN roles ro ON ro.code=ra.role_code WHERE ra.user_id=u.id AND ra.organization_id=active.organization_id AND ro.scope='ORGANIZATION' AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now())),'{}'),
       COALESCE((SELECT array_agg(DISTINCT rp.permission_code ORDER BY rp.permission_code) FROM role_assignments ra JOIN roles ro ON ro.code=ra.role_code JOIN role_permissions rp ON rp.role_code=ra.role_code WHERE ra.user_id=u.id AND ra.organization_id=active.organization_id AND ro.scope='ORGANIZATION' AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now())),'{}'),
       COALESCE((
         SELECT jsonb_agg(jsonb_build_object(
           'unit_id', scoped.unit_id,
           'role', scoped.role_code,
           'permissions', scoped.permissions
         ) ORDER BY scoped.unit_id,scoped.role_code)
         FROM (
           SELECT ra.unit_id,ra.role_code,array_agg(DISTINCT rp.permission_code ORDER BY rp.permission_code) AS permissions
           FROM role_assignments ra
           JOIN roles ro ON ro.code=ra.role_code AND ro.scope='UNIT'
           JOIN role_permissions rp ON rp.role_code=ra.role_code
           WHERE ra.user_id=u.id AND ra.organization_id=active.organization_id AND ra.unit_id IS NOT NULL
             AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now())
           GROUP BY ra.unit_id,ra.role_code
         ) scoped
       ),'[]'::jsonb),
       COALESCE((SELECT array_agg(DISTINCT ra.role_code ORDER BY ra.role_code) FROM role_assignments ra JOIN roles ro ON ro.code=ra.role_code WHERE ra.user_id=u.id AND ra.organization_id IS NULL AND ro.scope='PERSONAL' AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now())),'{}'),
       COALESCE((SELECT array_agg(DISTINCT rp.permission_code ORDER BY rp.permission_code) FROM role_assignments ra JOIN roles ro ON ro.code=ra.role_code JOIN role_permissions rp ON rp.role_code=ra.role_code WHERE ra.user_id=u.id AND ra.organization_id IS NULL AND ro.scope='PERSONAL' AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now())),'{}'),
       COALESCE((SELECT array_agg(DISTINCT ra.role_code ORDER BY ra.role_code) FROM role_assignments ra JOIN roles ro ON ro.code=ra.role_code WHERE ra.user_id=u.id AND ra.organization_id IS NULL AND ro.scope='PLATFORM' AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now())),'{}'),
       COALESCE((SELECT array_agg(DISTINCT rp.permission_code ORDER BY rp.permission_code) FROM role_assignments ra JOIN roles ro ON ro.code=ra.role_code JOIN role_permissions rp ON rp.role_code=ra.role_code WHERE ra.user_id=u.id AND ra.organization_id IS NULL AND ro.scope='PLATFORM' AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now())),'{}'),
       COALESCE((SELECT av.version FROM authorization_versions av WHERE av.user_id=u.id AND av.scope=CASE WHEN active.organization_id IS NULL THEN 'PERSONAL' ELSE 'ORGANIZATION' END AND av.scope_id=COALESCE(active.organization_id,'00000000-0000-0000-0000-000000000000'::uuid)),1),
       COALESCE((SELECT version FROM authorization_versions WHERE user_id=u.id AND scope='PERSONAL' AND scope_id='00000000-0000-0000-0000-000000000000'::uuid),1),
       COALESCE((SELECT version FROM authorization_versions WHERE user_id=u.id AND scope='PLATFORM' AND scope_id='00000000-0000-0000-0000-000000000000'::uuid),1),
       COALESCE((SELECT version FROM authorization_versions WHERE user_id=u.id AND scope='ORGANIZATION' AND scope_id=active.organization_id),1),
       u.mobile_e164,u.mobile_verified_at IS NOT NULL
FROM identities i
JOIN users u ON u.id=i.user_id AND u.status='ACTIVE'
JOIN user_sessions s ON s.user_id=u.id AND s.issuer=i.issuer AND s.session_id=$3
JOIN user_active_contexts active ON active.user_id=u.id
LEFT JOIN organizations o ON o.id=active.organization_id
LEFT JOIN memberships m ON m.user_id=u.id AND m.organization_id=active.organization_id AND m.status='ACTIVE'
LEFT JOIN membership_units mu ON mu.membership_id=m.id
LEFT JOIN organization_unit_closure c ON c.organization_id=m.organization_id AND c.descendant_id=mu.unit_id
WHERE i.issuer=$1 AND i.subject=$2 AND (active.organization_id IS NULL OR o.status='ACTIVE')
GROUP BY u.id,s.session_id,active.context_type,active.organization_id,active.context_version`, principal.Issuer, principal.Subject, principal.SessionID).Scan(
		&resolved.UserID, &resolved.SessionID, &resolved.ContextType, &resolved.OrganizationID,
		&resolved.MembershipVersion, &resolved.ContextVersion, &resolved.UnitIDs, &resolved.AncestorUnitIDs,
		&resolved.Roles, &resolved.Permissions, &resolved.UnitAuthorizationBindings,
		&resolved.PersonalRoles, &resolved.PersonalPermissions,
		&resolved.PlatformRoles, &resolved.PlatformPermissions, &resolved.AuthorizationVersion,
		&resolved.PersonalAuthorizationVersion, &resolved.PlatformAuthorizationVersion,
		&resolved.OrganizationAuthorizationVersion, &resolved.Mobile, &resolved.MobileVerified,
	)
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

func (r *PostgresRepository) ListOrganizations(ctx context.Context, principal identity.Principal) ([]Organization, error) {
	rows, err := r.pool.Query(ctx, `
SELECT o.id, o.name, o.status, o.version, array_agg(rb.role ORDER BY rb.role)
FROM identities i
JOIN memberships m ON m.user_id = i.user_id AND m.status = 'ACTIVE'
JOIN organizations o ON o.id = m.organization_id AND o.status = 'ACTIVE'
JOIN role_bindings rb ON rb.membership_id = m.id AND rb.revoked_at IS NULL AND rb.starts_at<=now() AND (rb.expires_at IS NULL OR rb.expires_at>now())
WHERE i.issuer = $1 AND i.subject = $2
GROUP BY o.id, o.name, o.status, o.version
ORDER BY o.name, o.id`, principal.Issuer, principal.Subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	organizations := make([]Organization, 0)
	for rows.Next() {
		var organization Organization
		if err := rows.Scan(&organization.ID, &organization.Name, &organization.Status, &organization.Version, &organization.Roles); err != nil {
			return nil, err
		}
		organizations = append(organizations, organization)
	}
	return organizations, rows.Err()
}

func (r *PostgresRepository) SetActiveContext(ctx context.Context, principal identity.Principal, organizationID *uuid.UUID, expectedVersion int64) (Context, error) {
	if expectedVersion < 1 {
		return Context{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Context{}, err
	}
	defer tx.Rollback(ctx)
	var userID uuid.UUID
	var membershipVersion, contextVersion int64
	var currentOrganizationID *uuid.UUID
	var currentType string
	err = tx.QueryRow(ctx, `
SELECT u.id,u.membership_version,active.context_type,active.organization_id,active.context_version
FROM identities i JOIN users u ON u.id=i.user_id
JOIN user_sessions session ON session.user_id=u.id AND session.issuer=i.issuer AND session.session_id=$3
JOIN user_active_contexts active ON active.user_id=u.id
WHERE i.issuer=$1 AND i.subject=$2 AND u.status='ACTIVE'
FOR UPDATE OF active`, principal.Issuer, principal.Subject, principal.SessionID).Scan(&userID, &membershipVersion, &currentType, &currentOrganizationID, &contextVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return Context{}, ErrForbidden
	}
	if err != nil {
		return Context{}, err
	}
	if contextVersion != expectedVersion {
		return Context{}, ErrConflict
	}
	contextType := "PERSONAL"
	if organizationID != nil {
		var allowed bool
		err = tx.QueryRow(ctx, `
SELECT EXISTS(
  SELECT 1 FROM memberships m JOIN organizations o ON o.id = m.organization_id
  WHERE m.user_id = $1 AND m.organization_id = $2 AND m.status = 'ACTIVE' AND o.status = 'ACTIVE'
)`, userID, *organizationID).Scan(&allowed)
		if err != nil {
			return Context{}, err
		}
		if !allowed {
			return Context{}, ErrForbidden
		}
		contextType = "ORGANIZATION"
	}
	changed := currentType != contextType || !uuidPointerEqual(currentOrganizationID, organizationID)
	now := r.now()
	if changed {
		contextVersion++
		if _, err := tx.Exec(ctx, `UPDATE user_active_contexts SET context_type=$2,organization_id=$3,context_version=$4,updated_at=$5 WHERE user_id=$1`, userID, contextType, organizationID, contextVersion, now); err != nil {
			return Context{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE user_sessions SET active_context_type=$2,active_organization_id=$3,membership_version=$4,last_seen_at=CASE WHEN issuer=$5 AND session_id=$6 THEN $7 ELSE last_seen_at END WHERE user_id=$1`, userID, contextType, organizationID, membershipVersion, principal.Issuer, principal.SessionID, now); err != nil {
			return Context{}, err
		}
		if err := appendAudit(ctx, tx, userID, principal.SessionID, organizationID, "user.active_context.set", "user_active_context", userID.String(), "SUCCEEDED", map[string]any{"context_type": contextType, "previous_context_type": currentType, "context_version": contextVersion}); err != nil {
			return Context{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Context{}, err
	}
	var authorizationVersion, personalAuthorizationVersion, platformAuthorizationVersion, organizationAuthorizationVersion int64
	var permissions []string
	err = r.pool.QueryRow(ctx, `
SELECT COALESCE((SELECT version FROM authorization_versions WHERE user_id=$1 AND scope=CASE WHEN $2::uuid IS NULL THEN 'PERSONAL' ELSE 'ORGANIZATION' END AND scope_id=COALESCE($2::uuid,'00000000-0000-0000-0000-000000000000'::uuid)),1),
       COALESCE((SELECT version FROM authorization_versions WHERE user_id=$1 AND scope='PERSONAL' AND scope_id='00000000-0000-0000-0000-000000000000'::uuid),1),
       COALESCE((SELECT version FROM authorization_versions WHERE user_id=$1 AND scope='PLATFORM' AND scope_id='00000000-0000-0000-0000-000000000000'::uuid),1),
       COALESCE((SELECT version FROM authorization_versions WHERE user_id=$1 AND scope='ORGANIZATION' AND scope_id=$2),1),
       COALESCE((
         SELECT array_agg(DISTINCT rp.permission_code ORDER BY rp.permission_code)
         FROM role_assignments ra JOIN roles ro ON ro.code=ra.role_code
         JOIN role_permissions rp ON rp.role_code=ra.role_code
         WHERE ra.user_id=$1 AND ra.organization_id=$2
           AND (($2::uuid IS NULL AND ro.scope='PERSONAL') OR ($2::uuid IS NOT NULL AND ro.scope='ORGANIZATION'))
           AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now())
       ),'{}')`, userID, organizationID).Scan(
		&authorizationVersion, &personalAuthorizationVersion, &platformAuthorizationVersion,
		&organizationAuthorizationVersion, &permissions,
	)
	if err != nil {
		return Context{}, err
	}
	return Context{
		Type: contextType, OrganizationID: organizationID, MembershipVersion: membershipVersion,
		ContextVersion: contextVersion, AuthorizationVersion: authorizationVersion,
		PersonalAuthorizationVersion:     personalAuthorizationVersion,
		PlatformAuthorizationVersion:     platformAuthorizationVersion,
		OrganizationAuthorizationVersion: organizationAuthorizationVersion,
		Permissions:                      permissions,
	}, nil
}

func uuidPointerEqual(left, right *uuid.UUID) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func (r *PostgresRepository) CreateUnit(ctx context.Context, principal identity.Principal, organizationID uuid.UUID, parentID *uuid.UUID, name, idempotencyKey string) (UnitMutationResult, error) {
	name = strings.TrimSpace(name)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if name == "" || len([]rune(name)) > 120 || !validOrganizationUnitIdempotencyKey(idempotencyKey) {
		return UnitMutationResult{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return UnitMutationResult{}, err
	}
	defer tx.Rollback(ctx)
	userID, err := activeUserID(ctx, tx, principal)
	if err != nil {
		return UnitMutationResult{}, err
	}
	if err := requireOrganizationPermission(ctx, tx, userID, organizationID, parentID, PermissionOrganizationUnitsManage); err != nil {
		return UnitMutationResult{}, err
	}
	requestHash := organizationUnitRequestHash("CREATE", uuid.Nil, parentID, name, 0)
	replayed, err := lockOrganizationUnitCommand(ctx, tx, userID, organizationID, "CREATE", idempotencyKey, requestHash)
	if err != nil {
		return UnitMutationResult{}, err
	}
	if replayed != nil {
		return *replayed, tx.Commit(ctx)
	}
	var organizationStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM organizations WHERE id=$1 FOR UPDATE`, organizationID).Scan(&organizationStatus); err != nil {
		return UnitMutationResult{}, err
	}
	if organizationStatus != "ACTIVE" {
		return UnitMutationResult{}, ErrConflict
	}
	if parentID != nil {
		var found bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organization_units WHERE organization_id = $1 AND id = $2 AND status = 'ACTIVE')`, organizationID, *parentID).Scan(&found); err != nil {
			return UnitMutationResult{}, err
		}
		if !found {
			return UnitMutationResult{}, ErrInvalidHierarchy
		}
	}
	now := r.now()
	unit := Unit{ID: uuid.Must(uuid.NewV7()), OrganizationID: organizationID, ParentID: parentID, Name: name, Status: "ACTIVE", Version: 1}
	if _, err := tx.Exec(ctx, `INSERT INTO organization_units(id, organization_id, parent_id, name, status, created_at, updated_at) VALUES ($1, $2, $3, $4, 'ACTIVE', $5, $5)`, unit.ID, organizationID, parentID, name, now); err != nil {
		return UnitMutationResult{}, mapConstraintError(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO organization_unit_closure(organization_id, ancestor_id, descendant_id, depth) VALUES ($1, $2, $2, 0)`, organizationID, unit.ID); err != nil {
		return UnitMutationResult{}, err
	}
	if parentID != nil {
		if _, err := tx.Exec(ctx, `
INSERT INTO organization_unit_closure(organization_id, ancestor_id, descendant_id, depth)
SELECT organization_id, ancestor_id, $3, depth + 1
		FROM organization_unit_closure WHERE organization_id = $1 AND descendant_id = $2`, organizationID, *parentID, unit.ID); err != nil {
			return UnitMutationResult{}, err
		}
	}
	structureVersion, err := nextOrganizationStructureVersion(ctx, tx, organizationID, now)
	if err != nil {
		return UnitMutationResult{}, err
	}
	result := UnitMutationResult{Unit: unit, StructureVersion: structureVersion}
	if err := saveOrganizationUnitCommand(ctx, tx, userID, organizationID, "CREATE", idempotencyKey, requestHash, result, now); err != nil {
		return UnitMutationResult{}, err
	}
	if err := appendAudit(ctx, tx, userID, principal.SessionID, &organizationID, "organization_unit.create", "organization_unit", unit.ID.String(), "SUCCEEDED", map[string]any{"parent_id": parentID, "idempotency_key": idempotencyKey, "structure_version": structureVersion}); err != nil {
		return UnitMutationResult{}, err
	}
	return result, tx.Commit(ctx)
}

func (r *PostgresRepository) MoveUnit(ctx context.Context, principal identity.Principal, organizationID, unitID uuid.UUID, newParentID *uuid.UUID, expectedStructureVersion int64, idempotencyKey string) (UnitMutationResult, error) {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if expectedStructureVersion < 1 || !validOrganizationUnitIdempotencyKey(idempotencyKey) {
		return UnitMutationResult{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return UnitMutationResult{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, organizationID.String()); err != nil {
		return UnitMutationResult{}, err
	}
	userID, err := activeUserID(ctx, tx, principal)
	if err != nil {
		return UnitMutationResult{}, err
	}
	if err := requireOrganizationPermission(ctx, tx, userID, organizationID, &unitID, PermissionOrganizationUnitsManage); err != nil {
		return UnitMutationResult{}, err
	}
	if err := requireOrganizationPermission(ctx, tx, userID, organizationID, newParentID, PermissionOrganizationUnitsManage); err != nil {
		return UnitMutationResult{}, err
	}
	requestHash := organizationUnitRequestHash("MOVE", unitID, newParentID, "", expectedStructureVersion)
	replayed, err := lockOrganizationUnitCommand(ctx, tx, userID, organizationID, "MOVE", idempotencyKey, requestHash)
	if err != nil {
		return UnitMutationResult{}, err
	}
	if replayed != nil {
		return *replayed, tx.Commit(ctx)
	}
	var organizationStatus string
	var structureVersion int64
	if err := tx.QueryRow(ctx, `SELECT status,structure_version FROM organizations WHERE id=$1 FOR UPDATE`, organizationID).Scan(&organizationStatus, &structureVersion); err != nil {
		return UnitMutationResult{}, err
	}
	if organizationStatus != "ACTIVE" || structureVersion != expectedStructureVersion {
		return UnitMutationResult{}, ErrConflict
	}
	var unit Unit
	err = tx.QueryRow(ctx, `SELECT id, organization_id, parent_id, name, status, version FROM organization_units WHERE organization_id = $1 AND id = $2 FOR UPDATE`, organizationID, unitID).Scan(&unit.ID, &unit.OrganizationID, &unit.ParentID, &unit.Name, &unit.Status, &unit.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return UnitMutationResult{}, ErrNotFound
	}
	if err != nil {
		return UnitMutationResult{}, err
	}
	if unit.Status != "ACTIVE" {
		return UnitMutationResult{}, ErrConflict
	}
	if newParentID != nil {
		if *newParentID == unitID {
			return UnitMutationResult{}, ErrInvalidHierarchy
		}
		var isDescendant bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organization_unit_closure WHERE organization_id = $1 AND ancestor_id = $2 AND descendant_id = $3)`, organizationID, unitID, *newParentID).Scan(&isDescendant); err != nil {
			return UnitMutationResult{}, err
		}
		if isDescendant {
			return UnitMutationResult{}, ErrInvalidHierarchy
		}
		var parentActive bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organization_units WHERE organization_id=$1 AND id=$2 AND status='ACTIVE')`, organizationID, *newParentID).Scan(&parentActive); err != nil {
			return UnitMutationResult{}, err
		}
		if !parentActive {
			return UnitMutationResult{}, ErrInvalidHierarchy
		}
	}
	if (unit.ParentID == nil && newParentID == nil) || (unit.ParentID != nil && newParentID != nil && *unit.ParentID == *newParentID) {
		result := UnitMutationResult{Unit: unit, StructureVersion: structureVersion}
		if err := saveOrganizationUnitCommand(ctx, tx, userID, organizationID, "MOVE", idempotencyKey, requestHash, result, r.now()); err != nil {
			return UnitMutationResult{}, err
		}
		return result, tx.Commit(ctx)
	}
	_, err = tx.Exec(ctx, `
DELETE FROM organization_unit_closure c
WHERE c.organization_id = $1
  AND c.descendant_id IN (SELECT descendant_id FROM organization_unit_closure WHERE organization_id = $1 AND ancestor_id = $2)
  AND c.ancestor_id NOT IN (SELECT descendant_id FROM organization_unit_closure WHERE organization_id = $1 AND ancestor_id = $2)`, organizationID, unitID)
	if err != nil {
		return UnitMutationResult{}, err
	}
	if newParentID != nil {
		_, err = tx.Exec(ctx, `
INSERT INTO organization_unit_closure(organization_id, ancestor_id, descendant_id, depth)
SELECT $1, parent_path.ancestor_id, subtree.descendant_id, parent_path.depth + subtree.depth + 1
FROM organization_unit_closure parent_path
CROSS JOIN organization_unit_closure subtree
WHERE parent_path.organization_id = $1 AND parent_path.descendant_id = $3
  AND subtree.organization_id = $1 AND subtree.ancestor_id = $2`, organizationID, unitID, *newParentID)
		if err != nil {
			return UnitMutationResult{}, err
		}
	}
	now := r.now()
	unit.ParentID = newParentID
	unit.Version++
	if _, err := tx.Exec(ctx, `UPDATE organization_units SET parent_id = $3, version = version + 1, updated_at = $4 WHERE organization_id = $1 AND id = $2`, organizationID, unitID, newParentID, now); err != nil {
		return UnitMutationResult{}, mapConstraintError(err)
	}
	structureVersion, err = nextOrganizationStructureVersion(ctx, tx, organizationID, now)
	if err != nil {
		return UnitMutationResult{}, err
	}
	result := UnitMutationResult{Unit: unit, StructureVersion: structureVersion}
	if err := saveOrganizationUnitCommand(ctx, tx, userID, organizationID, "MOVE", idempotencyKey, requestHash, result, now); err != nil {
		return UnitMutationResult{}, err
	}
	if err := appendAudit(ctx, tx, userID, principal.SessionID, &organizationID, "organization_unit.move", "organization_unit", unitID.String(), "SUCCEEDED", map[string]any{"new_parent_id": newParentID, "idempotency_key": idempotencyKey, "structure_version": structureVersion}); err != nil {
		return UnitMutationResult{}, err
	}
	return result, tx.Commit(ctx)
}

func loadUserByIdentity(ctx context.Context, tx pgx.Tx, principal identity.Principal, commerceProviderID string) (Me, error) {
	var me Me
	err := tx.QueryRow(ctx, `SELECT u.id,u.user_code,u.status,u.username,u.display_name,u.mobile_e164,u.mobile_verified_at IS NOT NULL,COALESCE(cm.status,'NOT_CONFIGURED'),u.membership_version FROM identities i JOIN users u ON u.id=i.user_id LEFT JOIN commerce_customer_mappings cm ON cm.subject_type='USER' AND cm.subject_id=u.id AND cm.provider_id=$3 WHERE i.issuer=$1 AND i.subject=$2`, principal.Issuer, principal.Subject, commerceProviderID).Scan(&me.UserID, &me.UserCode, &me.Status, &me.Username, &me.DisplayName, &me.Mobile, &me.MobileVerified, &me.CommerceCustomerStatus, &me.ActiveContext.MembershipVersion)
	me.ActiveContext.Type = "PERSONAL"
	me.UserCode = FormatUserCode(me.UserCode)
	me.ActiveContext.ContextVersion = me.ActiveContext.MembershipVersion
	me.ActiveContext.AuthorizationVersion = 1
	me.ActiveContext.PersonalAuthorizationVersion = 1
	me.ActiveContext.PlatformAuthorizationVersion = 1
	me.ActiveContext.OrganizationAuthorizationVersion = 1
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
func activeOrClosingUserID(ctx context.Context, tx pgx.Tx, principal identity.Principal) (uuid.UUID, error) {
	var userID uuid.UUID
	err := tx.QueryRow(ctx, `SELECT u.id FROM identities i JOIN users u ON u.id=i.user_id WHERE i.issuer=$1 AND i.subject=$2 AND u.status IN ('ACTIVE','CLOSING')`, principal.Issuer, principal.Subject).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrForbidden
	}
	return userID, err
}

func loadOrganization(ctx context.Context, tx pgx.Tx, organizationID uuid.UUID) (Organization, error) {
	var organization Organization
	err := tx.QueryRow(ctx, `SELECT id, name, status, version FROM organizations WHERE id = $1`, organizationID).Scan(&organization.ID, &organization.Name, &organization.Status, &organization.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return Organization{}, ErrNotFound
	}
	return organization, err
}

func validateInvitationRoles(roles []string) ([]string, error) {
	allowed := map[string]bool{"ORG_ADMIN": true, "BILLING_ADMIN": true, "MEMBER": true, "AUDITOR": true}
	unique := make(map[string]bool)
	result := make([]string, 0, len(roles)+1)
	for _, role := range roles {
		role = strings.ToUpper(strings.TrimSpace(role))
		if !allowed[role] {
			return nil, ErrConflict
		}
		if !unique[role] {
			unique[role] = true
			result = append(result, role)
		}
	}
	if !unique["MEMBER"] {
		result = append(result, "MEMBER")
	}
	return result, nil
}

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
