package account

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"net/http"
	"sort"
	"strings"
	"time"
)

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
	if err := appendMembershipChanged(ctx, tx, membershipID, organization.ID, ownerUserID, "ACTIVE", 1); err != nil {
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
	affectedUsers, err := updateAdminMemberships(ctx, tx, `UPDATE memberships SET status=$2,version=version+1,updated_at=$4 WHERE organization_id=$1 AND status=$3 RETURNING id,organization_id,user_id,status,version`, organizationID, toMembershipStatus, fromMembershipStatus, now)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET membership_version=membership_version+1,updated_at=$2 WHERE id=ANY($1)`, affectedUsers, now); err != nil {
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

// The handler's confirmed remote deletion tuple is for observedVersion+1.
// Fence completion against a concurrent local lifecycle change.
func (r *PostgresRepository) AdminDeleteOrganization(ctx context.Context, principal identity.Principal, organizationID uuid.UUID, observedVersion int64, reason string) error {
	if observedVersion < 1 {
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
		if currentVersion != observedVersion+1 {
			return ErrConflict
		}
		return tx.Commit(ctx)
	}
	if status != "DISABLED" || currentVersion != observedVersion {
		return ErrConflict
	}
	now := r.now()
	affectedUsers, err := updateAdminMemberships(ctx, tx, `UPDATE memberships SET status='REVOKED',version=version+1,updated_at=$2 WHERE organization_id=$1 AND status<>'REVOKED' RETURNING id,organization_id,user_id,status,version`, organizationID, now)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET membership_version=membership_version+1,updated_at=$2 WHERE id=ANY($1)`, affectedUsers, now); err != nil {
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
	// OWNER history remains present. The original deferred owner constraint
	// sees the terminal organization before these grants are softly revoked.
	if _, err := tx.Exec(ctx, `UPDATE role_bindings binding SET revoked_at=$2,revoked_by=$3 FROM memberships member WHERE binding.membership_id=member.id AND member.organization_id=$1 AND binding.revoked_at IS NULL`, organizationID, now, actorID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM membership_units attached USING memberships member WHERE attached.membership_id=member.id AND member.organization_id=$1`, organizationID); err != nil {
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

// updateAdminMemberships records each changed membership's authoritative version
// in the same transaction as the admin mutation. Consume rows before appending
// events because pgx cannot execute another query while transaction rows are open.
func updateAdminMemberships(ctx context.Context, tx pgx.Tx, query string, arguments ...any) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	type changedMembership struct {
		id, organizationID, userID uuid.UUID
		status                     string
		version                    int64
	}
	var changes []changedMembership
	for rows.Next() {
		var changed changedMembership
		if err := rows.Scan(&changed.id, &changed.organizationID, &changed.userID, &changed.status, &changed.version); err != nil {
			rows.Close()
			return nil, err
		}
		changes = append(changes, changed)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	users := make([]uuid.UUID, 0, len(changes))
	for _, changed := range changes {
		if err := appendMembershipChanged(ctx, tx, changed.id, changed.organizationID, changed.userID, changed.status, changed.version); err != nil {
			return nil, err
		}
		users = append(users, changed.userID)
	}
	return users, nil
}
