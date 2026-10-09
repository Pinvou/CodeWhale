package account

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"strings"
)

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
         WHERE ra.user_id=$1 AND ra.organization_id IS NOT DISTINCT FROM $2::uuid
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
		Permissions:                      filterF01Permissions(permissions),
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

func activeOrClosingUserID(ctx context.Context, tx pgx.Tx, principal identity.Principal) (uuid.UUID, error) {
	var userID uuid.UUID
	err := tx.QueryRow(ctx, `SELECT u.id FROM identities i JOIN users u ON u.id=i.user_id WHERE i.issuer=$1 AND i.subject=$2 AND u.status IN ('ACTIVE','CLOSING')`, principal.Issuer, principal.Subject).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrForbidden
	}
	return userID, err
}
