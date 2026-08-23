package account

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
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
	pool                   *pgxpool.Pool
	now                    func() time.Time
	walletPolicyVersion    string
	walletProvisioningSLA  time.Duration
	commerceProviderID     string
	commerceAdapterVersion string
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
func (r *PostgresRepository) Close()                         { r.pool.Close() }

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
INSERT INTO user_sessions(issuer, session_id, user_id, active_context_type, membership_version, last_seen_at, created_at)
VALUES ($1, $2, $3, 'PERSONAL', $4, $5, $5)
ON CONFLICT (issuer, session_id) DO UPDATE
SET last_seen_at = EXCLUDED.last_seen_at
WHERE user_sessions.user_id = EXCLUDED.user_id`,
		principal.Issuer, principal.SessionID, user.UserID, user.ActiveContext.MembershipVersion, now)
	if err != nil {
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
       COALESCE(cm.status,'NOT_CONFIGURED'), s.active_context_type, s.active_organization_id, u.membership_version,
       COALESCE((SELECT av.version FROM authorization_versions av WHERE av.user_id=u.id AND av.scope=CASE WHEN s.active_organization_id IS NULL THEN 'PLATFORM' ELSE 'ORGANIZATION' END AND av.scope_id=COALESCE(s.active_organization_id,'00000000-0000-0000-0000-000000000000'::uuid)),1)
FROM identities i
JOIN users u ON u.id = i.user_id
JOIN user_sessions s ON s.user_id = u.id AND s.issuer = i.issuer AND s.session_id = $3
LEFT JOIN commerce_customer_mappings cm ON cm.subject_type='USER' AND cm.subject_id=u.id AND cm.provider_id=$4
WHERE i.issuer = $1 AND i.subject = $2`, principal.Issuer, principal.Subject, principal.SessionID, r.commerceProviderID).Scan(
		&me.UserID, &me.UserCode, &me.Status, &me.Username, &me.DisplayName, &me.Mobile, &me.MobileVerified, &me.CommerceCustomerStatus, &me.ActiveContext.Type, &organizationID, &me.ActiveContext.MembershipVersion, &me.ActiveContext.AuthorizationVersion,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Me{}, ErrNotFound
	}
	me.ActiveContext.OrganizationID = organizationID
	me.UserCode = FormatUserCode(me.UserCode)
	me.ActiveContext.ContextVersion = me.ActiveContext.MembershipVersion
	return me, err
}

func (r *PostgresRepository) ResolveAuthorizationContext(ctx context.Context, principal identity.Principal) (AuthorizationContext, error) {
	var resolved AuthorizationContext
	err := r.pool.QueryRow(ctx, `
SELECT u.id,s.session_id,s.active_context_type,s.active_organization_id,s.membership_version,
       COALESCE(array_agg(DISTINCT mu.unit_id) FILTER (WHERE mu.unit_id IS NOT NULL),'{}'),
       COALESCE(array_agg(DISTINCT c.ancestor_id) FILTER (WHERE c.ancestor_id IS NOT NULL),'{}'),
       COALESCE((SELECT array_agg(DISTINCT ra.role_code ORDER BY ra.role_code) FROM role_assignments ra WHERE ra.user_id=u.id AND ra.organization_id=s.active_organization_id AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now())),'{}'),
       COALESCE((SELECT array_agg(DISTINCT rp.permission_code ORDER BY rp.permission_code) FROM role_assignments ra JOIN role_permissions rp ON rp.role_code=ra.role_code WHERE ra.user_id=u.id AND ra.organization_id=s.active_organization_id AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now())),'{}'),
       COALESCE((SELECT array_agg(DISTINCT ra.role_code ORDER BY ra.role_code) FROM role_assignments ra WHERE ra.user_id=u.id AND ra.organization_id IS NULL AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now())),'{}'),
       COALESCE((SELECT array_agg(DISTINCT rp.permission_code ORDER BY rp.permission_code) FROM role_assignments ra JOIN role_permissions rp ON rp.role_code=ra.role_code WHERE ra.user_id=u.id AND ra.organization_id IS NULL AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now())),'{}'),
       COALESCE((SELECT av.version FROM authorization_versions av WHERE av.user_id=u.id AND av.scope=CASE WHEN s.active_organization_id IS NULL THEN 'PLATFORM' ELSE 'ORGANIZATION' END AND av.scope_id=COALESCE(s.active_organization_id,'00000000-0000-0000-0000-000000000000'::uuid)),1),
       u.mobile_e164,u.mobile_verified_at IS NOT NULL
FROM identities i
JOIN users u ON u.id=i.user_id AND u.status='ACTIVE'
JOIN user_sessions s ON s.user_id=u.id AND s.issuer=i.issuer AND s.session_id=$3
LEFT JOIN organizations o ON o.id=s.active_organization_id
LEFT JOIN memberships m ON m.user_id=u.id AND m.organization_id=s.active_organization_id AND m.status='ACTIVE'
LEFT JOIN membership_units mu ON mu.membership_id=m.id
LEFT JOIN organization_unit_closure c ON c.organization_id=m.organization_id AND c.descendant_id=mu.unit_id
WHERE i.issuer=$1 AND i.subject=$2 AND (s.active_organization_id IS NULL OR o.status='ACTIVE')
GROUP BY u.id,s.session_id,s.active_context_type,s.active_organization_id,s.membership_version`, principal.Issuer, principal.Subject, principal.SessionID).Scan(
		&resolved.UserID, &resolved.SessionID, &resolved.ContextType, &resolved.OrganizationID, &resolved.MembershipVersion, &resolved.UnitIDs, &resolved.AncestorUnitIDs, &resolved.Roles, &resolved.Permissions, &resolved.PlatformRoles, &resolved.PlatformPermissions, &resolved.AuthorizationVersion, &resolved.Mobile, &resolved.MobileVerified,
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
		return AuthorizationContext{}, ErrNotFound
	}
	if err != nil {
		return AuthorizationContext{}, err
	}
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
	resolved.ContextVersion = resolved.MembershipVersion
	if resolved.AuthorizationVersion < 1 {
		resolved.AuthorizationVersion = 1
	}
	if resolved.Permissions == nil {
		resolved.Permissions = []string{}
	}
	if resolved.PlatformRoles == nil {
		resolved.PlatformRoles = []string{}
	}
	if resolved.PlatformPermissions == nil {
		resolved.PlatformPermissions = []string{}
	}
	return resolved, nil
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

func (r *PostgresRepository) SetActiveContext(ctx context.Context, principal identity.Principal, organizationID *uuid.UUID) (Context, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Context{}, err
	}
	defer tx.Rollback(ctx)
	var userID uuid.UUID
	var version int64
	err = tx.QueryRow(ctx, `
SELECT u.id, u.membership_version
FROM identities i JOIN users u ON u.id = i.user_id
WHERE i.issuer = $1 AND i.subject = $2 AND u.status = 'ACTIVE'`, principal.Issuer, principal.Subject).Scan(&userID, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return Context{}, ErrForbidden
	}
	if err != nil {
		return Context{}, err
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
	command, err := tx.Exec(ctx, `
UPDATE user_sessions
SET active_context_type = $4, active_organization_id = $5, membership_version = $6, last_seen_at = $7
WHERE issuer = $1 AND session_id = $2 AND user_id = $3`,
		principal.Issuer, principal.SessionID, userID, contextType, organizationID, version, r.now())
	if err != nil {
		return Context{}, err
	}
	if command.RowsAffected() != 1 {
		return Context{}, ErrNotFound
	}
	if err := appendAudit(ctx, tx, userID, principal.SessionID, organizationID, "session.active_context.set", "session", principal.SessionID, "SUCCEEDED", map[string]any{"context_type": contextType}); err != nil {
		return Context{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Context{}, err
	}
	var authorizationVersion int64
	err = r.pool.QueryRow(ctx, `SELECT COALESCE((SELECT version FROM authorization_versions WHERE user_id=$1 AND scope=CASE WHEN $2::uuid IS NULL THEN 'PLATFORM' ELSE 'ORGANIZATION' END AND scope_id=COALESCE($2::uuid,'00000000-0000-0000-0000-000000000000'::uuid)),1)`, userID, organizationID).Scan(&authorizationVersion)
	if err != nil {
		return Context{}, err
	}
	return Context{Type: contextType, OrganizationID: organizationID, MembershipVersion: version, ContextVersion: version, AuthorizationVersion: authorizationVersion}, nil
}

func (r *PostgresRepository) CreateUnit(ctx context.Context, principal identity.Principal, organizationID uuid.UUID, parentID *uuid.UUID, name string) (Unit, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 120 {
		return Unit{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Unit{}, err
	}
	defer tx.Rollback(ctx)
	userID, err := activeUserID(ctx, tx, principal)
	if err != nil {
		return Unit{}, err
	}
	if err := requireOrganizationPermission(ctx, tx, userID, organizationID, parentID, PermissionOrganizationUnitsManage); err != nil {
		return Unit{}, err
	}
	if parentID != nil {
		var found bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organization_units WHERE organization_id = $1 AND id = $2 AND status = 'ACTIVE')`, organizationID, *parentID).Scan(&found); err != nil {
			return Unit{}, err
		}
		if !found {
			return Unit{}, ErrInvalidHierarchy
		}
	}
	now := r.now()
	unit := Unit{ID: uuid.Must(uuid.NewV7()), OrganizationID: organizationID, ParentID: parentID, Name: name, Status: "ACTIVE", Version: 1}
	if _, err := tx.Exec(ctx, `INSERT INTO organization_units(id, organization_id, parent_id, name, status, created_at, updated_at) VALUES ($1, $2, $3, $4, 'ACTIVE', $5, $5)`, unit.ID, organizationID, parentID, name, now); err != nil {
		return Unit{}, mapConstraintError(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO organization_unit_closure(organization_id, ancestor_id, descendant_id, depth) VALUES ($1, $2, $2, 0)`, organizationID, unit.ID); err != nil {
		return Unit{}, err
	}
	if parentID != nil {
		if _, err := tx.Exec(ctx, `
INSERT INTO organization_unit_closure(organization_id, ancestor_id, descendant_id, depth)
SELECT organization_id, ancestor_id, $3, depth + 1
FROM organization_unit_closure WHERE organization_id = $1 AND descendant_id = $2`, organizationID, *parentID, unit.ID); err != nil {
			return Unit{}, err
		}
	}
	if err := appendAudit(ctx, tx, userID, principal.SessionID, &organizationID, "organization_unit.create", "organization_unit", unit.ID.String(), "SUCCEEDED", map[string]any{"parent_id": parentID}); err != nil {
		return Unit{}, err
	}
	return unit, tx.Commit(ctx)
}

func (r *PostgresRepository) MoveUnit(ctx context.Context, principal identity.Principal, organizationID, unitID uuid.UUID, newParentID *uuid.UUID, expectedVersion int64) (Unit, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Unit{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, organizationID.String()); err != nil {
		return Unit{}, err
	}
	userID, err := activeUserID(ctx, tx, principal)
	if err != nil {
		return Unit{}, err
	}
	if err := requireOrganizationPermission(ctx, tx, userID, organizationID, &unitID, PermissionOrganizationUnitsManage); err != nil {
		return Unit{}, err
	}
	var unit Unit
	err = tx.QueryRow(ctx, `SELECT id, organization_id, parent_id, name, status, version FROM organization_units WHERE organization_id = $1 AND id = $2 FOR UPDATE`, organizationID, unitID).Scan(&unit.ID, &unit.OrganizationID, &unit.ParentID, &unit.Name, &unit.Status, &unit.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return Unit{}, ErrNotFound
	}
	if err != nil {
		return Unit{}, err
	}
	if unit.Version != expectedVersion {
		return Unit{}, ErrConflict
	}
	if newParentID != nil {
		if *newParentID == unitID {
			return Unit{}, ErrInvalidHierarchy
		}
		var isDescendant bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organization_unit_closure WHERE organization_id = $1 AND ancestor_id = $2 AND descendant_id = $3)`, organizationID, unitID, *newParentID).Scan(&isDescendant); err != nil {
			return Unit{}, err
		}
		if isDescendant {
			return Unit{}, ErrInvalidHierarchy
		}
	}
	_, err = tx.Exec(ctx, `
DELETE FROM organization_unit_closure c
WHERE c.organization_id = $1
  AND c.descendant_id IN (SELECT descendant_id FROM organization_unit_closure WHERE organization_id = $1 AND ancestor_id = $2)
  AND c.ancestor_id NOT IN (SELECT descendant_id FROM organization_unit_closure WHERE organization_id = $1 AND ancestor_id = $2)`, organizationID, unitID)
	if err != nil {
		return Unit{}, err
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
			return Unit{}, err
		}
	}
	unit.ParentID = newParentID
	unit.Version++
	if _, err := tx.Exec(ctx, `UPDATE organization_units SET parent_id = $3, version = version + 1, updated_at = $4 WHERE organization_id = $1 AND id = $2`, organizationID, unitID, newParentID, r.now()); err != nil {
		return Unit{}, mapConstraintError(err)
	}
	if err := appendAudit(ctx, tx, userID, principal.SessionID, &organizationID, "organization_unit.move", "organization_unit", unitID.String(), "SUCCEEDED", map[string]any{"new_parent_id": newParentID}); err != nil {
		return Unit{}, err
	}
	return unit, tx.Commit(ctx)
}

func (r *PostgresRepository) CreateInvitation(ctx context.Context, principal identity.Principal, organizationID uuid.UUID, email string, roles []string, expiresAt time.Time) (Invitation, error) {
	email = normalizeEmail(email)
	roles, err := validateInvitationRoles(roles)
	if err != nil || email == "" || !expiresAt.After(r.now()) || expiresAt.After(r.now().Add(30*24*time.Hour)) {
		return Invitation{}, ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Invitation{}, err
	}
	defer tx.Rollback(ctx)
	userID, err := activeUserID(ctx, tx, principal)
	if err != nil {
		return Invitation{}, err
	}
	if err := requireOrganizationPermission(ctx, tx, userID, organizationID, nil, PermissionOrganizationMembersManage); err != nil {
		return Invitation{}, err
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return Invitation{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	hash := sha256.Sum256([]byte(token))
	rolesJSON, _ := json.Marshal(roles)
	now := r.now()
	invitation := Invitation{ID: uuid.Must(uuid.NewV7()), OrganizationID: organizationID, Email: email, Roles: roles, ExpiresAt: expiresAt.UTC(), Token: token}
	_, err = tx.Exec(ctx, `
INSERT INTO invitations(id, organization_id, email_normalized, token_hash, status, roles, invited_by, expires_at, created_at, updated_at)
VALUES ($1, $2, $3, $4, 'PENDING', $5, $6, $7, $8, $8)`, invitation.ID, organizationID, email, hash[:], rolesJSON, userID, invitation.ExpiresAt, now)
	if err != nil {
		return Invitation{}, mapConstraintError(err)
	}
	if err := appendAudit(ctx, tx, userID, principal.SessionID, &organizationID, "invitation.create", "invitation", invitation.ID.String(), "SUCCEEDED", map[string]any{"roles": roles, "expires_at": invitation.ExpiresAt}); err != nil {
		return Invitation{}, err
	}
	return invitation, tx.Commit(ctx)
}

func (r *PostgresRepository) AcceptInvitation(ctx context.Context, principal identity.Principal, token string) (Organization, error) {
	hash := sha256.Sum256([]byte(strings.TrimSpace(token)))
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Organization{}, err
	}
	defer tx.Rollback(ctx)
	userID, err := activeUserID(ctx, tx, principal)
	if err != nil {
		return Organization{}, err
	}
	var invitationID, organizationID uuid.UUID
	var email, status string
	var rolesJSON []byte
	var expiresAt time.Time
	var acceptedBy *uuid.UUID
	err = tx.QueryRow(ctx, `
SELECT i.id,i.organization_id,i.email_normalized,i.status,i.roles,i.expires_at,i.accepted_by
FROM invitations i JOIN organizations o ON o.id=i.organization_id AND o.status='ACTIVE'
WHERE i.token_hash=$1 FOR UPDATE OF i`, hash[:]).Scan(&invitationID, &organizationID, &email, &status, &rolesJSON, &expiresAt, &acceptedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return Organization{}, ErrNotFound
	}
	if err != nil {
		return Organization{}, err
	}
	if status == "ACCEPTED" && acceptedBy != nil && *acceptedBy == userID {
		return loadOrganization(ctx, tx, organizationID)
	}
	if status != "PENDING" {
		return Organization{}, ErrConflict
	}
	if !expiresAt.After(r.now()) {
		_, _ = tx.Exec(ctx, `UPDATE invitations SET status = 'EXPIRED', updated_at = $2 WHERE id = $1`, invitationID, r.now())
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return Organization{}, commitErr
		}
		return Organization{}, ErrInvitationExpired
	}
	if normalizeEmail(principal.Email) == "" || normalizeEmail(principal.Email) != email {
		return Organization{}, ErrForbidden
	}
	var roles []string
	if err := json.Unmarshal(rolesJSON, &roles); err != nil {
		return Organization{}, err
	}
	now := r.now()
	membershipID := uuid.Must(uuid.NewV7())
	var membershipVersion int64
	err = tx.QueryRow(ctx, `
INSERT INTO memberships(id, organization_id, user_id, status, joined_at, updated_at)
VALUES ($1, $2, $3, 'ACTIVE', $4, $4)
ON CONFLICT (organization_id, user_id) DO UPDATE
SET status = 'ACTIVE', version = memberships.version + 1, updated_at = EXCLUDED.updated_at
RETURNING id,version`, membershipID, organizationID, userID, now).Scan(&membershipID, &membershipVersion)
	if err != nil {
		return Organization{}, err
	}
	for _, role := range roles {
		_, err = tx.Exec(ctx, `INSERT INTO role_bindings(id, membership_id, role, created_by, created_at, grant_source) VALUES ($1, $2, $3, $4, $5, 'INVITATION') ON CONFLICT DO NOTHING`, uuid.Must(uuid.NewV7()), membershipID, role, userID, now)
		if err != nil {
			return Organization{}, err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE invitations SET status = 'ACCEPTED', accepted_by = $2, updated_at = $3 WHERE id = $1`, invitationID, userID, now)
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE users SET membership_version = membership_version + 1, updated_at = $2 WHERE id = $1`, userID, now)
	}
	if err != nil {
		return Organization{}, err
	}
	if err := appendAudit(ctx, tx, userID, principal.SessionID, &organizationID, "invitation.accept", "invitation", invitationID.String(), "SUCCEEDED", nil); err != nil {
		return Organization{}, err
	}
	if err := appendOutbox(ctx, tx, "account.membership.changed.v1", membershipID, membershipVersion, map[string]any{"organization_id": organizationID, "user_id": userID, "membership_status": "ACTIVE", "membership_version": membershipVersion}); err != nil {
		return Organization{}, err
	}
	organization, err := loadOrganization(ctx, tx, organizationID)
	if err != nil {
		return Organization{}, err
	}
	return organization, tx.Commit(ctx)
}

func (r *PostgresRepository) RevokeMembership(ctx context.Context, principal identity.Principal, organizationID, targetUserID uuid.UUID) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	actorID, err := activeUserID(ctx, tx, principal)
	if err != nil {
		return err
	}
	if err := requireOrganizationPermission(ctx, tx, actorID, organizationID, nil, PermissionOrganizationMembersManage); err != nil {
		return err
	}
	var membershipID uuid.UUID
	var membershipVersion int64
	var isOwner bool
	err = tx.QueryRow(ctx, `
SELECT m.id,m.version,EXISTS(SELECT 1 FROM role_bindings rb WHERE rb.membership_id = m.id AND rb.role = 'OWNER')
FROM memberships m WHERE m.organization_id = $1 AND m.user_id = $2 AND m.status = 'ACTIVE' FOR UPDATE`, organizationID, targetUserID).Scan(&membershipID, &membershipVersion, &isOwner)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if isOwner {
		var owners int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM role_bindings rb JOIN memberships m ON m.id = rb.membership_id WHERE m.organization_id = $1 AND m.status = 'ACTIVE' AND rb.role = 'OWNER'`, organizationID).Scan(&owners); err != nil {
			return err
		}
		if owners <= 1 {
			return ErrLastOwner
		}
	}
	now := r.now()
	membershipVersion++
	_, err = tx.Exec(ctx, `UPDATE memberships SET status = 'REVOKED', version = version + 1, updated_at = $2 WHERE id = $1`, membershipID, now)
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE users SET membership_version = membership_version + 1, updated_at = $2 WHERE id = $1`, targetUserID, now)
	}
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE user_sessions SET active_context_type = 'PERSONAL', active_organization_id = NULL, membership_version = membership_version + 1, last_seen_at = $3 WHERE user_id = $1 AND active_organization_id = $2`, targetUserID, organizationID, now)
	}
	if err != nil {
		return err
	}
	if err := appendAudit(ctx, tx, actorID, principal.SessionID, &organizationID, "membership.revoke", "membership", membershipID.String(), "SUCCEEDED", map[string]any{"target_user_id": targetUserID}); err != nil {
		return err
	}
	if err := appendOutbox(ctx, tx, "account.membership.changed.v1", membershipID, membershipVersion, map[string]any{"organization_id": organizationID, "user_id": targetUserID, "membership_status": "REVOKED", "membership_version": membershipVersion}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) TransferOwnership(ctx context.Context, principal identity.Principal, organizationID, targetUserID uuid.UUID) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	actorID, err := activeUserID(ctx, tx, principal)
	if err != nil {
		return err
	}
	var actorMembership, targetMembership uuid.UUID
	err = tx.QueryRow(ctx, `
SELECT m.id FROM memberships m
WHERE m.organization_id=$1 AND m.user_id=$2 AND m.status='ACTIVE'
  AND EXISTS(
    SELECT 1 FROM role_assignments ra JOIN role_permissions rp ON rp.role_code=ra.role_code
    WHERE ra.user_id=m.user_id AND ra.organization_id=m.organization_id
      AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now())
      AND ra.unit_id IS NULL AND rp.permission_code=$3
  )
FOR UPDATE OF m`, organizationID, actorID, PermissionOrganizationOwnerTransfer).Scan(&actorMembership)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrForbidden
	}
	if err != nil {
		return err
	}
	err = tx.QueryRow(ctx, `SELECT id FROM memberships WHERE organization_id = $1 AND user_id = $2 AND status = 'ACTIVE' FOR UPDATE`, organizationID, targetUserID).Scan(&targetMembership)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if actorID == targetUserID {
		return nil
	}
	now := r.now()
	_, err = tx.Exec(ctx, `DELETE FROM role_bindings WHERE membership_id = $1 AND role = 'OWNER'`, actorMembership)
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO role_bindings(id, membership_id, role, created_by, created_at, grant_source) VALUES ($1, $2, 'OWNER', $3, $4, 'OWNERSHIP_TRANSFER') ON CONFLICT DO NOTHING`, uuid.Must(uuid.NewV7()), targetMembership, actorID, now)
	}
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE users SET membership_version = membership_version + 1, updated_at = $2 WHERE id = ANY($1)`, []uuid.UUID{actorID, targetUserID}, now)
	}
	if err != nil {
		return err
	}
	if err := appendAudit(ctx, tx, actorID, principal.SessionID, &organizationID, "organization.ownership.transfer", "organization", organizationID.String(), "SUCCEEDED", map[string]any{"new_owner_user_id": targetUserID}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func loadUserByIdentity(ctx context.Context, tx pgx.Tx, principal identity.Principal, commerceProviderID string) (Me, error) {
	var me Me
	err := tx.QueryRow(ctx, `SELECT u.id,u.user_code,u.status,u.username,u.display_name,u.mobile_e164,u.mobile_verified_at IS NOT NULL,COALESCE(cm.status,'NOT_CONFIGURED'),u.membership_version FROM identities i JOIN users u ON u.id=i.user_id LEFT JOIN commerce_customer_mappings cm ON cm.subject_type='USER' AND cm.subject_id=u.id AND cm.provider_id=$3 WHERE i.issuer=$1 AND i.subject=$2`, principal.Issuer, principal.Subject, commerceProviderID).Scan(&me.UserID, &me.UserCode, &me.Status, &me.Username, &me.DisplayName, &me.Mobile, &me.MobileVerified, &me.CommerceCustomerStatus, &me.ActiveContext.MembershipVersion)
	me.ActiveContext.Type = "PERSONAL"
	me.UserCode = FormatUserCode(me.UserCode)
	me.ActiveContext.ContextVersion = me.ActiveContext.MembershipVersion
	me.ActiveContext.AuthorizationVersion = 1
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
