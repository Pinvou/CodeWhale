package account

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (r *PostgresRepository) ListUnits(ctx context.Context, principal identity.Principal, organizationID uuid.UUID) ([]Unit, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	actorID, err := activeUserID(ctx, tx, principal)
	if err != nil {
		return nil, err
	}
	if err := requireOrganizationPermission(ctx, tx, actorID, organizationID, nil, PermissionOrganizationUnitsRead); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT id,organization_id,parent_id,name,status,version FROM organization_units WHERE organization_id=$1 AND status='ACTIVE' ORDER BY parent_id NULLS FIRST,name,id`, organizationID)
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, tx.Commit(ctx)
}

func (r *PostgresRepository) ListMembers(ctx context.Context, principal identity.Principal, organizationID uuid.UUID) ([]Member, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	actorID, err := activeUserID(ctx, tx, principal)
	if err != nil {
		return nil, err
	}
	if err := requireOrganizationPermission(ctx, tx, actorID, organizationID, nil, PermissionOrganizationMembersRead); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
SELECT m.id,u.id,u.display_name,u.email,m.status,m.version,m.joined_at,
       ARRAY(SELECT DISTINCT rb.role FROM role_bindings rb WHERE rb.membership_id=m.id AND rb.revoked_at IS NULL AND rb.starts_at<=now() AND (rb.expires_at IS NULL OR rb.expires_at>now()) ORDER BY rb.role),
       ARRAY(SELECT mu.unit_id FROM membership_units mu WHERE mu.membership_id=m.id ORDER BY mu.unit_id),
       ARRAY(SELECT rb.unit_id FROM role_bindings rb WHERE rb.membership_id=m.id AND rb.role='UNIT_ADMIN' AND rb.revoked_at IS NULL AND rb.starts_at<=now() AND (rb.expires_at IS NULL OR rb.expires_at>now()) ORDER BY rb.unit_id)
FROM memberships m JOIN users u ON u.id=m.user_id
WHERE m.organization_id=$1 AND m.status='ACTIVE'
ORDER BY COALESCE(u.display_name,u.email,u.id::text),u.id`, organizationID)
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, tx.Commit(ctx)
}

func (r *PostgresRepository) ReplaceMemberRoles(ctx context.Context, principal identity.Principal, organizationID, targetUserID uuid.UUID, roles []string) (Member, error) {
	roles, err := validateManagedRoles(roles)
	if err != nil {
		return Member{}, err
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Member{}, err
	}
	defer tx.Rollback(ctx)
	actorID, err := activeUserID(ctx, tx, principal)
	if err != nil {
		return Member{}, err
	}
	if err := requireOrganizationPermission(ctx, tx, actorID, organizationID, nil, PermissionOrganizationRolesAssign); err != nil {
		return Member{}, err
	}
	var membershipID uuid.UUID
	var version int64
	err = tx.QueryRow(ctx, `SELECT id,version FROM memberships WHERE organization_id=$1 AND user_id=$2 AND status='ACTIVE' FOR UPDATE`, organizationID, targetUserID).Scan(&membershipID, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return Member{}, ErrNotFound
	}
	if err != nil {
		return Member{}, err
	}
	now := r.now()
	if _, err := tx.Exec(ctx, `UPDATE role_bindings SET revoked_at=$2,revoked_by=$3 WHERE membership_id=$1 AND role IN ('ORG_ADMIN','BILLING_ADMIN','MEMBER','AUDITOR') AND revoked_at IS NULL`, membershipID, now, actorID); err != nil {
		return Member{}, err
	}
	for _, role := range roles {
		if _, err := tx.Exec(ctx, `INSERT INTO role_bindings(id,membership_id,role,created_by,created_at,grant_source) VALUES($1,$2,$3,$4,$5,'ORGANIZATION_ADMIN')`, uuid.Must(uuid.NewV7()), membershipID, role, actorID, now); err != nil {
			return Member{}, err
		}
	}
	version++
	if _, err := tx.Exec(ctx, `UPDATE memberships SET version=$2,updated_at=$3 WHERE id=$1`, membershipID, version, now); err != nil {
		return Member{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET membership_version=membership_version+1,updated_at=$2 WHERE id=$1`, targetUserID, now); err != nil {
		return Member{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE user_sessions SET membership_version=membership_version+1,last_seen_at=$2 WHERE user_id=$1`, targetUserID, now); err != nil {
		return Member{}, err
	}
	if err := appendAudit(ctx, tx, actorID, principal.SessionID, &organizationID, "membership.roles.replace", "membership", membershipID.String(), "SUCCEEDED", map[string]any{"target_user_id": targetUserID, "roles": roles}); err != nil {
		return Member{}, err
	}
	if err := appendOutbox(ctx, tx, "account.membership.changed.v1", membershipID, version, map[string]any{"organization_id": organizationID, "user_id": targetUserID, "membership_status": "ACTIVE", "membership_version": version}); err != nil {
		return Member{}, err
	}
	member, err := getMember(ctx, tx, membershipID)
	if err != nil {
		return Member{}, err
	}
	return member, tx.Commit(ctx)
}

func validateManagedRoles(roles []string) ([]string, error) {
	allowed := []string{"ORG_ADMIN", "BILLING_ADMIN", "MEMBER", "AUDITOR"}
	seen := map[string]bool{}
	result := make([]string, 0, len(roles)+1)
	for _, role := range roles {
		role = strings.ToUpper(strings.TrimSpace(role))
		if !slices.Contains(allowed, role) {
			return nil, ErrConflict
		}
		if !seen[role] {
			seen[role] = true
			result = append(result, role)
		}
	}
	if !seen["MEMBER"] {
		result = append(result, "MEMBER")
	}
	slices.Sort(result)
	return result, nil
}

func getMember(ctx context.Context, database pgx.Tx, membershipID uuid.UUID) (Member, error) {
	var item Member
	err := database.QueryRow(ctx, `
SELECT m.id,u.id,u.display_name,u.email,m.status,m.version,m.joined_at,
       ARRAY(SELECT DISTINCT rb.role FROM role_bindings rb WHERE rb.membership_id=m.id AND rb.revoked_at IS NULL AND rb.starts_at<=now() AND (rb.expires_at IS NULL OR rb.expires_at>now()) ORDER BY rb.role),
       ARRAY(SELECT mu.unit_id FROM membership_units mu WHERE mu.membership_id=m.id ORDER BY mu.unit_id),
       ARRAY(SELECT rb.unit_id FROM role_bindings rb WHERE rb.membership_id=m.id AND rb.role='UNIT_ADMIN' AND rb.revoked_at IS NULL AND rb.starts_at<=now() AND (rb.expires_at IS NULL OR rb.expires_at>now()) ORDER BY rb.unit_id)
FROM memberships m JOIN users u ON u.id=m.user_id WHERE m.id=$1`, membershipID).Scan(&item.MembershipID, &item.UserID, &item.DisplayName, &item.Email, &item.Status, &item.Version, &item.JoinedAt, &item.Roles, &item.UnitIDs, &item.UnitAdminIDs)
	return item, err
}
