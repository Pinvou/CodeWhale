package account

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

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
	AND ra.revoked_at IS NULL AND ra.starts_at<=statement_timestamp() AND (ra.expires_at IS NULL OR ra.expires_at>statement_timestamp())
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
    AND actor_role.revoked_at IS NULL AND actor_role.starts_at<=statement_timestamp() AND (actor_role.expires_at IS NULL OR actor_role.expires_at>statement_timestamp())
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

// Fence only a new mutation's final authority check. Production member/role
// revocation locks the membership before updating its authorization projection.
// A Serializable transaction with an older snapshot must conflict here rather
// than authorize a write from stale role facts.
func requireOrganizationMutationPermission(ctx context.Context, tx pgx.Tx, userID, organizationID uuid.UUID, unitID *uuid.UUID, permission string) error {
	var membershipID uuid.UUID
	err := tx.QueryRow(ctx, `SELECT member.id FROM memberships member JOIN users actor ON actor.id=member.user_id AND actor.status='ACTIVE' WHERE member.user_id=$1 AND member.organization_id=$2 AND member.status='ACTIVE' FOR SHARE OF member,actor`, userID, organizationID).Scan(&membershipID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrForbidden
	}
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT ra.id FROM role_assignments ra
JOIN role_permissions rp ON rp.role_code=ra.role_code AND rp.permission_code=$4
WHERE ra.user_id=$1 AND ra.organization_id=$2 AND ra.revoked_at IS NULL
AND ra.starts_at<=statement_timestamp() AND (ra.expires_at IS NULL OR ra.expires_at>statement_timestamp())
AND (ra.unit_id IS NULL OR ($3::uuid IS NOT NULL AND EXISTS(
 SELECT 1 FROM organization_unit_closure c WHERE c.organization_id=$2 AND c.ancestor_id=ra.unit_id AND c.descendant_id=$3
))) ORDER BY ra.id FOR SHARE OF ra`, userID, organizationID, unitID, permission)
	if err != nil {
		return err
	}
	for rows.Next() {
		var bindingID uuid.UUID
		if err := rows.Scan(&bindingID); err != nil {
			rows.Close()
			return err
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// Lock acquisition can cross a scheduled boundary. Keep the existing final
	// statement-time predicate after every authority lock is held.
	return requireOrganizationPermission(ctx, tx, userID, organizationID, unitID, permission)
}

// Lock the actor's authority before rechecking the final target-scoped predicate.
// Serializable conflicts prevent stale membership/role snapshots from mutating.
func requireOrganizationMemberMutationPermission(ctx context.Context, tx pgx.Tx, userID, organizationID, targetUserID uuid.UUID, permission string) error {
	var membershipID uuid.UUID
	err := tx.QueryRow(ctx, `SELECT member.id FROM memberships member JOIN users actor ON actor.id=member.user_id AND actor.status='ACTIVE' WHERE member.user_id=$1 AND member.organization_id=$2 AND member.status='ACTIVE' FOR SHARE OF member,actor`, userID, organizationID).Scan(&membershipID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrForbidden
	}
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT ra.id FROM role_assignments ra JOIN role_permissions rp ON rp.role_code=ra.role_code AND rp.permission_code=$3 WHERE ra.user_id=$1 AND ra.organization_id=$2 AND ra.revoked_at IS NULL ORDER BY ra.id FOR SHARE OF ra`, userID, organizationID, canonicalPermissionCode(permission))
	if err != nil {
		return err
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	return requireOrganizationMemberPermission(ctx, tx, userID, organizationID, targetUserID, permission)
}

// Replays require current authority over the immutable affected scope. Removed
// membership_units cannot be used as a current access projection. Legacy receipts
// without scope remain replayable only by a current organization-wide actor.
func requireMembershipRemovalReplayPermission(ctx context.Context, tx pgx.Tx, actorID, organizationID uuid.UUID, unitIDs []uuid.UUID) error {
	if err := requireOrganizationMutationPermission(ctx, tx, actorID, organizationID, nil, PermissionOrganizationMembersManage); err == nil {
		return nil
	} else if !errors.Is(err, ErrForbidden) {
		return err
	}
	if len(unitIDs) == 0 {
		return ErrForbidden
	}
	rows, err := tx.Query(ctx, `SELECT ra.id FROM role_assignments ra JOIN role_permissions rp ON rp.role_code=ra.role_code AND rp.permission_code=$3 WHERE ra.user_id=$1 AND ra.organization_id=$2 AND ra.revoked_at IS NULL ORDER BY ra.id FOR SHARE OF ra`, actorID, organizationID, canonicalPermissionCode(PermissionOrganizationMembersManage))
	if err != nil {
		return err
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	return requireMembershipAffectedScopePermission(ctx, tx, actorID, organizationID, unitIDs)
}

// Read-only scope check for the initial permission fence. The replay caller
// above still owns all original authority locks and the final recheck.
func requireMembershipAffectedScopePermission(ctx context.Context, tx pgx.Tx, actorID, organizationID uuid.UUID, unitIDs []uuid.UUID) error {
	var allowed bool
	err := tx.QueryRow(ctx, `WITH authorized_roots AS (
 SELECT role.unit_id FROM role_assignments role
 JOIN users actor ON actor.id=role.user_id AND actor.status='ACTIVE'
 JOIN memberships member ON member.user_id=role.user_id AND member.organization_id=role.organization_id AND member.status='ACTIVE'
 JOIN organizations organization ON organization.id=role.organization_id AND organization.status='ACTIVE'
 JOIN role_permissions permission ON permission.role_code=role.role_code AND permission.permission_code=$4
 WHERE role.user_id=$1 AND role.organization_id=$2 AND role.revoked_at IS NULL AND role.starts_at<=statement_timestamp() AND (role.expires_at IS NULL OR role.expires_at>statement_timestamp())
 ) SELECT cardinality($3::uuid[])>0 AND NOT EXISTS(
 SELECT 1 FROM unnest($3::uuid[]) affected(unit_id) WHERE NOT EXISTS(
 SELECT 1 FROM authorized_roots root JOIN organization_unit_closure closure ON closure.organization_id=$2 AND closure.ancestor_id=root.unit_id AND closure.descendant_id=affected.unit_id
 WHERE root.unit_id IS NOT NULL
 ))`, actorID, organizationID, unitIDs, canonicalPermissionCode(PermissionOrganizationMembersManage)).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrForbidden
	}
	return nil
}
