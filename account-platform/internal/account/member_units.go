package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Ordinary attachment is not a UNIT_ADMIN role grant. A nil optional input is
// preserved for legacy producers; an explicitly empty set means unscoped.
func normalizedMemberUnits(input []uuid.UUID) ([]uuid.UUID, error) {
	if input == nil {
		return nil, nil
	}
	result := make([]uuid.UUID, 0, len(input))
	for _, id := range input {
		if id == uuid.Nil {
			return nil, ErrConflict
		}
		if !slices.Contains(result, id) {
			result = append(result, id)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].String() < result[j].String() })
	return result, nil
}

func readMemberUnits(ctx context.Context, tx pgx.Tx, membershipID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `SELECT unit_id FROM membership_units WHERE membership_id=$1 ORDER BY unit_id`, membershipID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]uuid.UUID, 0)
	for rows.Next() {
		var id uuid.UUID
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func writeMemberUnits(ctx context.Context, tx pgx.Tx, membershipID uuid.UUID, ids []uuid.UUID, now time.Time) error {
	if _, err := tx.Exec(ctx, `DELETE FROM membership_units WHERE membership_id=$1`, membershipID); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.Exec(ctx, `INSERT INTO membership_units(membership_id,unit_id,created_at) VALUES($1,$2,$3)`, membershipID, id, now); err != nil {
			return err
		}
	}
	return nil
}

type memberUnitsReceipt struct {
	MembershipMutationResult
	AffectedUnitIDs []uuid.UUID `json:"affected_unit_ids"`
}

func (r *PostgresRepository) ReplaceMemberUnits(ctx context.Context, principal identity.Principal, organizationID, targetID uuid.UUID, expectedVersion int64, input []uuid.UUID, key string, platformAdmin bool) (MembershipMutationResult, error) {
	units, err := normalizedMemberUnits(input)
	if err != nil || input == nil || expectedVersion < 1 || !validOrganizationUnitIdempotencyKey(key) {
		return MembershipMutationResult{}, ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return MembershipMutationResult{}, err
	}
	defer tx.Rollback(ctx)
	actorID, err := activeUserID(ctx, tx, principal)
	if err != nil {
		return MembershipMutationResult{}, err
	}
	if platformAdmin {
		if err = requireMemberUnitPlatformPermission(ctx, tx, actorID); err != nil {
			return MembershipMutationResult{}, err
		}
	}
	var orgStatus string
	if err = tx.QueryRow(ctx, `SELECT status FROM organizations WHERE id=$1 FOR UPDATE`, organizationID).Scan(&orgStatus); errors.Is(err, pgx.ErrNoRows) {
		return MembershipMutationResult{}, ErrNotFound
	} else if err != nil {
		return MembershipMutationResult{}, err
	}
	if orgStatus != "ACTIVE" {
		return MembershipMutationResult{}, ErrConflict
	}
	if err = validateAdminUnits(ctx, tx, organizationID, units); err != nil {
		return MembershipMutationResult{}, err
	}
	canonical, _ := json.Marshal(map[string]any{"target": targetID, "expected_version": expectedVersion, "unit_ids": units})
	digest := sha256.Sum256(canonical)
	hash := hex.EncodeToString(digest[:])
	var receipt memberUnitsReceipt
	found, err := lockMembershipCommand(ctx, tx, actorID, organizationID, "UNITS_REPLACE", key, hash, &receipt)
	if err != nil {
		return MembershipMutationResult{}, err
	}
	if found {
		if receipt.OrganizationID != organizationID || receipt.Member.UserID != targetID {
			return MembershipMutationResult{}, ErrConflict
		}
		if !platformAdmin {
			if err = requireMembershipRemovalReplayPermission(ctx, tx, actorID, organizationID, receipt.AffectedUnitIDs); err != nil {
				return MembershipMutationResult{}, err
			}
		}
		member, err := getMember(ctx, tx, receipt.Member.MembershipID)
		if err != nil {
			return MembershipMutationResult{}, err
		}
		return MembershipMutationResult{OrganizationID: organizationID, Member: member, Replayed: true}, tx.Commit(ctx)
	}
	var membershipID uuid.UUID
	var version int64
	if err = tx.QueryRow(ctx, `SELECT m.id,m.version FROM memberships m JOIN users u ON u.id=m.user_id AND u.status='ACTIVE' WHERE m.organization_id=$1 AND m.user_id=$2 AND m.status='ACTIVE' FOR UPDATE OF m`, organizationID, targetID).Scan(&membershipID, &version); errors.Is(err, pgx.ErrNoRows) {
		return MembershipMutationResult{}, ErrNotFound
	} else if err != nil {
		return MembershipMutationResult{}, err
	}
	if version != expectedVersion {
		return MembershipMutationResult{}, ErrConflict
	}
	old, err := readMemberUnits(ctx, tx, membershipID)
	if err != nil {
		return MembershipMutationResult{}, err
	}
	affected, _ := normalizedMemberUnits(append(append([]uuid.UUID{}, old...), units...))
	if !platformAdmin {
		if err = requireOrganizationMemberMutationPermission(ctx, tx, actorID, organizationID, targetID, PermissionOrganizationMembersManage); err != nil {
			return MembershipMutationResult{}, err
		}
		if err = requireMembershipRemovalReplayPermission(ctx, tx, actorID, organizationID, affected); err != nil {
			return MembershipMutationResult{}, err
		}
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return MembershipMutationResult{}, err
	}
	if err = writeMemberUnits(ctx, tx, membershipID, units, now); err != nil {
		return MembershipMutationResult{}, err
	}
	version++
	if _, err = tx.Exec(ctx, `UPDATE memberships SET version=$2,updated_at=$3 WHERE id=$1`, membershipID, version, now); err != nil {
		return MembershipMutationResult{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE users SET membership_version=membership_version+1,updated_at=$2 WHERE id=$1`, targetID, now); err != nil {
		return MembershipMutationResult{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE user_sessions SET membership_version=membership_version+1,last_seen_at=$2 WHERE user_id=$1`, targetID, now); err != nil {
		return MembershipMutationResult{}, err
	}
	if err = appendAudit(ctx, tx, actorID, principal.SessionID, &organizationID, "membership.units.replace", "membership", membershipID.String(), "SUCCEEDED", map[string]any{"unit_ids": units, "expected_version": expectedVersion}); err != nil {
		return MembershipMutationResult{}, err
	}
	if err = appendOutbox(ctx, tx, "account.membership.changed.v1", membershipID, version, map[string]any{"organization_id": organizationID, "user_id": targetID, "membership_status": "ACTIVE", "membership_version": version}); err != nil {
		return MembershipMutationResult{}, err
	}
	member, err := getMember(ctx, tx, membershipID)
	if err != nil {
		return MembershipMutationResult{}, err
	}
	result := MembershipMutationResult{OrganizationID: organizationID, Member: member}
	if err = saveMembershipCommand(ctx, tx, actorID, organizationID, targetID, membershipID, "UNITS_REPLACE", key, hash, memberUnitsReceipt{MembershipMutationResult: result, AffectedUnitIDs: affected}, now); err != nil {
		return MembershipMutationResult{}, err
	}
	return result, tx.Commit(ctx)
}

// Platform input has a separate permission fence; a caller-supplied flag never
// substitutes for a currently active, locked assignment.
func requireMemberUnitPlatformPermission(ctx context.Context, tx pgx.Tx, actor uuid.UUID) error {
	var active uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 AND status='ACTIVE' FOR SHARE`, actor).Scan(&active); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrForbidden
		}
		return err
	}

	rows, err := tx.Query(ctx, `SELECT assignment.id FROM role_assignments assignment JOIN role_permissions permission ON permission.role_code=assignment.role_code AND permission.permission_code=$2 WHERE assignment.user_id=$1 AND assignment.organization_id IS NULL AND assignment.unit_id IS NULL AND assignment.revoked_at IS NULL ORDER BY assignment.id FOR SHARE OF assignment`, actor, canonicalPermissionCode(PermissionPlatformOrganizationsMembersManage))
	if err != nil {
		return err
	}
	for rows.Next() {
		var id uuid.UUID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	var allowed bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM role_assignments assignment JOIN users actor ON actor.id=assignment.user_id AND actor.status='ACTIVE' JOIN role_permissions permission ON permission.role_code=assignment.role_code AND permission.permission_code=$2 WHERE assignment.user_id=$1 AND assignment.organization_id IS NULL AND assignment.unit_id IS NULL AND assignment.revoked_at IS NULL AND assignment.starts_at<=statement_timestamp() AND (assignment.expires_at IS NULL OR assignment.expires_at>statement_timestamp()))`, actor, canonicalPermissionCode(PermissionPlatformOrganizationsMembersManage)).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrForbidden
	}
	return nil
}
