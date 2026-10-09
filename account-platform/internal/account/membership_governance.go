package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func membershipRequestHash(commandType string, targetUserID uuid.UUID, roles []string, unitIDs []uuid.UUID) string {
	roleCopy := append([]string(nil), roles...)
	sort.Strings(roleCopy)
	unitCopy := append([]uuid.UUID(nil), unitIDs...)
	sort.Slice(unitCopy, func(i, j int) bool { return unitCopy[i].String() < unitCopy[j].String() })
	canonical, _ := json.Marshal(map[string]any{"version": 1, "command_type": commandType, "target_user_id": targetUserID, "roles": roleCopy, "unit_ids": unitCopy})
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:])
}

func lockMembershipCommand(ctx context.Context, tx pgx.Tx, actorID, organizationID uuid.UUID, commandType, idempotencyKey, requestHash string, result any) (bool, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "membership-command:"+actorID.String()+":"+organizationID.String()+":"+commandType+":"+idempotencyKey); err != nil {
		return false, err
	}
	var storedHash string
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT request_hash,result FROM membership_commands WHERE actor_user_id=$1 AND organization_id=$2 AND command_type=$3 AND idempotency_key=$4`, actorID, organizationID, commandType, idempotencyKey).Scan(&storedHash, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if storedHash != requestHash {
		return false, ErrConflict
	}
	if err := json.Unmarshal(raw, result); err != nil {
		return false, err
	}
	return true, nil
}

func saveMembershipCommand(ctx context.Context, tx pgx.Tx, actorID, organizationID, targetUserID, membershipID uuid.UUID, commandType, idempotencyKey, requestHash string, result any, now time.Time) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO membership_commands(actor_user_id,organization_id,target_user_id,command_type,idempotency_key,request_hash,membership_id,result,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, actorID, organizationID, targetUserID, commandType, strings.TrimSpace(idempotencyKey), requestHash, membershipID, raw, now)
	return err
}

func markMembershipResultReplayed(result *MembershipMutationResult) { result.Replayed = true }
func markOwnershipResultReplayed(result *OwnershipTransferResult)   { result.Replayed = true }

func (r *PostgresRepository) AdminAddMember(ctx context.Context, principal identity.Principal, organizationID, userID uuid.UUID, roles []string, unitAdminIDs []uuid.UUID, idempotencyKey string) (MembershipMutationResult, error) {
	return r.AdminAddMemberWithUnits(ctx, principal, organizationID, userID, roles, unitAdminIDs, nil, idempotencyKey)
}
func (r *PostgresRepository) AdminAddMemberWithUnits(ctx context.Context, principal identity.Principal, organizationID, userID uuid.UUID, roles []string, unitAdminIDs, input []uuid.UUID, key string) (MembershipMutationResult, error) {
	idempotencyKey := key
	units, err := normalizedMemberUnits(input)
	if err != nil {
		return MembershipMutationResult{}, err
	}

	roles, unitAdminIDs, err = normalizeAdminMemberRoles(roles, unitAdminIDs)
	if err != nil || !validOrganizationUnitIdempotencyKey(idempotencyKey) {
		return MembershipMutationResult{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return MembershipMutationResult{}, err
	}
	defer tx.Rollback(ctx)
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return MembershipMutationResult{}, err
	}
	if actorID == nil {
		return MembershipMutationResult{}, ErrForbidden
	}
	if units != nil {
		if err = requireMemberUnitPlatformPermission(ctx, tx, *actorID); err != nil {
			return MembershipMutationResult{}, err
		}
	}
	requestHash := membershipRequestHash("ADD", userID, roles, unitAdminIDs)
	if units != nil {
		encoded, _ := json.Marshal(map[string]any{"legacy_fingerprint": requestHash, "unit_ids": units})
		digest := sha256.Sum256(encoded)
		requestHash = hex.EncodeToString(digest[:])
	}
	var replay MembershipMutationResult
	found, err := lockMembershipCommand(ctx, tx, *actorID, organizationID, "ADD", strings.TrimSpace(idempotencyKey), requestHash, &replay)
	if err != nil {
		return MembershipMutationResult{}, err
	}
	if found {
		replay.Member, err = getMember(ctx, tx, replay.Member.MembershipID)
		if err != nil {
			return MembershipMutationResult{}, err
		}
		markMembershipResultReplayed(&replay)
		return replay, tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "membership:"+organizationID.String()+":"+userID.String()); err != nil {
		return MembershipMutationResult{}, err
	}
	var organizationStatus, userStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM organizations WHERE id=$1 FOR UPDATE`, organizationID).Scan(&organizationStatus); err != nil {
		return MembershipMutationResult{}, err
	}
	if err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&userStatus); err != nil {
		return MembershipMutationResult{}, err
	}
	if organizationStatus != "ACTIVE" || userStatus != "ACTIVE" {
		return MembershipMutationResult{}, ErrConflict
	}
	if err := validateAdminUnits(ctx, tx, organizationID, append(append([]uuid.UUID{}, unitAdminIDs...), units...)); err != nil {
		return MembershipMutationResult{}, err
	}
	var membershipID uuid.UUID
	var status string
	err = tx.QueryRow(ctx, `SELECT id,status FROM memberships WHERE organization_id=$1 AND user_id=$2 FOR UPDATE`, organizationID, userID).Scan(&membershipID, &status)
	now := r.now()
	changed := true
	if errors.Is(err, pgx.ErrNoRows) {
		membershipID = uuid.Must(uuid.NewV7())
		if _, err := tx.Exec(ctx, `INSERT INTO memberships(id,organization_id,user_id,status,joined_at,updated_at) VALUES($1,$2,$3,'ACTIVE',$4,$4)`, membershipID, organizationID, userID, now); err != nil {
			return MembershipMutationResult{}, mapConstraintError(err)
		}
	} else if err != nil {
		return MembershipMutationResult{}, err
	} else if status == "ACTIVE" {
		changed = false
	} else if _, err := tx.Exec(ctx, `UPDATE memberships SET status='ACTIVE',version=version+1,updated_at=$2 WHERE id=$1`, membershipID, now); err != nil {
		return MembershipMutationResult{}, err
	}
	var version int64

	if !changed && units != nil {
		existing, err := readMemberUnits(ctx, tx, membershipID)
		if err != nil {
			return MembershipMutationResult{}, err
		}
		if !slices.Equal(existing, units) {
			return MembershipMutationResult{}, ErrConflict
		}
	}
	if changed {
		if units != nil {
			if err = writeMemberUnits(ctx, tx, membershipID, units, now); err != nil {
				return MembershipMutationResult{}, err
			}
		}
		version, err = replaceAdminMemberRoles(ctx, tx, membershipID, userID, *actorID, roles, unitAdminIDs, now)
		if err != nil {
			return MembershipMutationResult{}, err
		}
		if err := appendAdminAudit(ctx, tx, actorID, principal, &organizationID, "admin.membership.add", "membership", membershipID.String(), map[string]any{"user_id": userID, "roles": roles, "unit_admin_unit_ids": unitAdminIDs, "idempotency_key": idempotencyKey}); err != nil {
			return MembershipMutationResult{}, err
		}
		if err := appendOutbox(ctx, tx, "account.membership.changed.v1", membershipID, version, map[string]any{"organization_id": organizationID, "user_id": userID, "membership_status": "ACTIVE", "membership_version": version}); err != nil {
			return MembershipMutationResult{}, err
		}
	}
	member, err := getMember(ctx, tx, membershipID)
	if err != nil {
		return MembershipMutationResult{}, err
	}
	result := MembershipMutationResult{OrganizationID: organizationID, Member: member, Replayed: !changed}
	stored := result
	stored.Replayed = false
	if err := saveMembershipCommand(ctx, tx, *actorID, organizationID, userID, membershipID, "ADD", idempotencyKey, requestHash, stored, now); err != nil {
		return MembershipMutationResult{}, err
	}
	return result, tx.Commit(ctx)
}

func (r *PostgresRepository) RevokeMembership(ctx context.Context, principal identity.Principal, organizationID, targetUserID uuid.UUID, idempotencyKey string) (MembershipMutationResult, error) {
	return r.removeMembership(ctx, principal, organizationID, targetUserID, idempotencyKey, false)
}

func (r *PostgresRepository) AdminRemoveMember(ctx context.Context, principal identity.Principal, organizationID, targetUserID uuid.UUID, idempotencyKey string) (MembershipMutationResult, error) {
	return r.removeMembership(ctx, principal, organizationID, targetUserID, idempotencyKey, true)
}

func (r *PostgresRepository) removeMembership(ctx context.Context, principal identity.Principal, organizationID, targetUserID uuid.UUID, idempotencyKey string, platformAdmin bool) (MembershipMutationResult, error) {
	if !validOrganizationUnitIdempotencyKey(idempotencyKey) {
		return MembershipMutationResult{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return MembershipMutationResult{}, err
	}
	defer tx.Rollback(ctx)
	var actorID uuid.UUID
	if platformAdmin {
		resolved, err := adminActorID(ctx, tx, principal)
		if err != nil || resolved == nil {
			if err != nil {
				return MembershipMutationResult{}, err
			}
			return MembershipMutationResult{}, ErrForbidden
		}
		actorID = *resolved
	} else {
		actorID, err = activeUserID(ctx, tx, principal)
		if err != nil {
			return MembershipMutationResult{}, err
		}
	}
	requestHash := membershipRequestHash("REMOVE", targetUserID, nil, nil)
	var replay membershipRemovalReceipt
	found, err := lockMembershipCommand(ctx, tx, actorID, organizationID, "REMOVE", strings.TrimSpace(idempotencyKey), requestHash, &replay)
	if err != nil {
		return MembershipMutationResult{}, err
	}
	if found {
		if replay.OrganizationID != organizationID || replay.Member.UserID != targetUserID || replay.Member.MembershipID == uuid.Nil {
			return MembershipMutationResult{}, ErrConflict
		}
		if !platformAdmin {
			if err := requireMembershipRemovalReplayPermission(ctx, tx, actorID, organizationID, replay.AffectedUnitIDs); err != nil {
				return MembershipMutationResult{}, err
			}
		}
		replay.Member, err = getMember(ctx, tx, replay.Member.MembershipID)
		if err != nil {
			return MembershipMutationResult{}, err
		}
		markMembershipResultReplayed(&replay.MembershipMutationResult)
		return replay.MembershipMutationResult, tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "membership:"+organizationID.String()+":"+targetUserID.String()); err != nil {
		return MembershipMutationResult{}, err
	}
	var membershipID uuid.UUID
	var status string
	var version int64
	err = tx.QueryRow(ctx, `SELECT id,status,version FROM memberships WHERE organization_id=$1 AND user_id=$2 FOR UPDATE`, organizationID, targetUserID).Scan(&membershipID, &status, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return MembershipMutationResult{}, ErrNotFound
	}
	if err != nil {
		return MembershipMutationResult{}, err
	}
	if !platformAdmin {
		if err := requireOrganizationMemberMutationPermission(ctx, tx, actorID, organizationID, targetUserID, PermissionOrganizationMembersManage); err != nil {
			return MembershipMutationResult{}, err
		}
	}
	var affectedUnitIDs []uuid.UUID
	units, err := tx.Query(ctx, `SELECT unit_id FROM membership_units WHERE membership_id=$1 ORDER BY unit_id`, membershipID)
	if err != nil {
		return MembershipMutationResult{}, err
	}
	for units.Next() {
		var id uuid.UUID
		if err := units.Scan(&id); err != nil {
			units.Close()
			return MembershipMutationResult{}, err
		}
		affectedUnitIDs = append(affectedUnitIDs, id)
	}
	units.Close()
	if err := units.Err(); err != nil {
		return MembershipMutationResult{}, err
	}
	now := r.now()
	changed := status != "REVOKED"
	if changed {
		var owner bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM role_bindings WHERE membership_id=$1 AND role='OWNER' AND revoked_at IS NULL AND starts_at<=statement_timestamp() AND (expires_at IS NULL OR expires_at>statement_timestamp()))`, membershipID).Scan(&owner); err != nil {
			return MembershipMutationResult{}, err
		}
		if owner {
			return MembershipMutationResult{}, ErrLastOwner
		}
		if _, err := tx.Exec(ctx, `UPDATE role_bindings SET revoked_at=$2,revoked_by=$3 WHERE membership_id=$1 AND revoked_at IS NULL`, membershipID, now, actorID); err != nil {
			return MembershipMutationResult{}, err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM membership_units WHERE membership_id=$1`, membershipID); err != nil {
			return MembershipMutationResult{}, err
		}
		version++
		if _, err := tx.Exec(ctx, `UPDATE memberships SET status='REVOKED',version=$2,updated_at=$3 WHERE id=$1`, membershipID, version, now); err != nil {
			return MembershipMutationResult{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET membership_version=membership_version+1,updated_at=$2 WHERE id=$1`, targetUserID, now); err != nil {
			return MembershipMutationResult{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE user_active_contexts SET context_type='PERSONAL',organization_id=NULL,context_version=context_version+1,updated_at=$3 WHERE user_id=$1 AND organization_id=$2`, targetUserID, organizationID, now); err != nil {
			return MembershipMutationResult{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE user_sessions SET active_context_type=CASE WHEN active_organization_id=$2 THEN 'PERSONAL' ELSE active_context_type END,active_organization_id=CASE WHEN active_organization_id=$2 THEN NULL ELSE active_organization_id END,membership_version=membership_version+1,last_seen_at=$3 WHERE user_id=$1`, targetUserID, organizationID, now); err != nil {
			return MembershipMutationResult{}, err
		}
		if platformAdmin {
			actor := &actorID
			if err := appendAdminAudit(ctx, tx, actor, principal, &organizationID, "admin.membership.remove", "membership", membershipID.String(), map[string]any{"user_id": targetUserID, "idempotency_key": idempotencyKey}); err != nil {
				return MembershipMutationResult{}, err
			}
		} else if err := appendAudit(ctx, tx, actorID, principal.SessionID, &organizationID, "membership.revoke", "membership", membershipID.String(), "SUCCEEDED", map[string]any{"target_user_id": targetUserID, "idempotency_key": idempotencyKey}); err != nil {
			return MembershipMutationResult{}, err
		}
		if err := appendOutbox(ctx, tx, "account.membership.changed.v1", membershipID, version, map[string]any{"organization_id": organizationID, "user_id": targetUserID, "membership_status": "REVOKED", "membership_version": version}); err != nil {
			return MembershipMutationResult{}, err
		}
	}
	member, err := getMember(ctx, tx, membershipID)
	if err != nil {
		return MembershipMutationResult{}, err
	}
	result := MembershipMutationResult{OrganizationID: organizationID, Member: member, Replayed: !changed}
	stored := result
	stored.Replayed = false
	if err := saveMembershipCommand(ctx, tx, actorID, organizationID, targetUserID, membershipID, "REMOVE", idempotencyKey, requestHash, membershipRemovalReceipt{MembershipMutationResult: stored, AffectedUnitIDs: affectedUnitIDs}, now); err != nil {
		return MembershipMutationResult{}, err
	}
	return result, tx.Commit(ctx)
}

func (r *PostgresRepository) TransferOwnership(ctx context.Context, principal identity.Principal, organizationID, targetUserID uuid.UUID, idempotencyKey string) (OwnershipTransferResult, error) {
	return r.transferOwnership(ctx, principal, organizationID, targetUserID, idempotencyKey, false)
}

func (r *PostgresRepository) AdminTransferOwnership(ctx context.Context, principal identity.Principal, organizationID, targetUserID uuid.UUID, idempotencyKey string) (OwnershipTransferResult, error) {
	return r.transferOwnership(ctx, principal, organizationID, targetUserID, idempotencyKey, true)
}

func (r *PostgresRepository) transferOwnership(ctx context.Context, principal identity.Principal, organizationID, targetUserID uuid.UUID, idempotencyKey string, platformAdmin bool) (OwnershipTransferResult, error) {
	if !validOrganizationUnitIdempotencyKey(idempotencyKey) {
		return OwnershipTransferResult{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return OwnershipTransferResult{}, err
	}
	defer tx.Rollback(ctx)
	var actorID uuid.UUID
	if platformAdmin {
		resolved, err := adminActorID(ctx, tx, principal)
		if err != nil || resolved == nil {
			if err != nil {
				return OwnershipTransferResult{}, err
			}
			return OwnershipTransferResult{}, ErrForbidden
		}
		actorID = *resolved
	} else {
		actorID, err = activeOrClosingUserID(ctx, tx, principal)
		if err != nil {
			return OwnershipTransferResult{}, err
		}
		if err := requireOrganizationPermission(ctx, tx, actorID, organizationID, nil, PermissionOrganizationOwnerTransfer); err != nil {
			return OwnershipTransferResult{}, err
		}
	}
	requestHash := membershipRequestHash("TRANSFER_OWNERSHIP", targetUserID, nil, nil)
	var replay OwnershipTransferResult
	found, err := lockMembershipCommand(ctx, tx, actorID, organizationID, "TRANSFER_OWNERSHIP", strings.TrimSpace(idempotencyKey), requestHash, &replay)
	if err != nil {
		return OwnershipTransferResult{}, err
	}
	if found {
		markOwnershipResultReplayed(&replay)
		return replay, tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, organizationID.String()); err != nil {
		return OwnershipTransferResult{}, err
	}
	var organizationStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM organizations WHERE id=$1 FOR UPDATE`, organizationID).Scan(&organizationStatus); err != nil {
		return OwnershipTransferResult{}, err
	}
	if organizationStatus != "ACTIVE" {
		return OwnershipTransferResult{}, ErrConflict
	}
	var targetMembershipID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT membership.id FROM memberships membership JOIN users u ON u.id=membership.user_id WHERE membership.organization_id=$1 AND membership.user_id=$2 AND membership.status='ACTIVE' AND u.status='ACTIVE' FOR UPDATE OF membership,u`, organizationID, targetUserID).Scan(&targetMembershipID); errors.Is(err, pgx.ErrNoRows) {
		return OwnershipTransferResult{}, ErrNotFound
	} else if err != nil {
		return OwnershipTransferResult{}, err
	}
	var previousMembershipID, previousOwnerID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT membership.id,membership.user_id FROM memberships membership JOIN role_bindings binding ON binding.membership_id=membership.id WHERE membership.organization_id=$1 AND membership.status<>'REVOKED' AND binding.role='OWNER' AND binding.revoked_at IS NULL AND binding.starts_at<=now() AND (binding.expires_at IS NULL OR binding.expires_at>now()) FOR UPDATE OF membership,binding`, organizationID).Scan(&previousMembershipID, &previousOwnerID)
	if err != nil {
		return OwnershipTransferResult{}, err
	}
	now := r.now()
	changed := previousOwnerID != targetUserID
	if changed {
		if _, err := tx.Exec(ctx, `UPDATE role_bindings SET revoked_at=$2,revoked_by=$3 WHERE membership_id=$1 AND role='OWNER' AND revoked_at IS NULL`, previousMembershipID, now, actorID); err != nil {
			return OwnershipTransferResult{}, err
		}
		// Keep the new OWNER effective at the exact timestamp used by the
		// deferred single-owner constraint. A later application timestamp would
		// leave the organization with zero effective owners at COMMIT.
		if _, err := tx.Exec(ctx, `INSERT INTO role_bindings(id,membership_id,role,created_by,created_at,starts_at,grant_source) VALUES($1,$2,'OWNER',$3,$4,transaction_timestamp(),'OWNERSHIP_TRANSFER')`, uuid.Must(uuid.NewV7()), targetMembershipID, actorID, now); err != nil {
			return OwnershipTransferResult{}, mapConstraintError(err)
		}
		membershipIDs := []uuid.UUID{previousMembershipID}
		if targetMembershipID != previousMembershipID {
			membershipIDs = append(membershipIDs, targetMembershipID)
		}
		if _, err := tx.Exec(ctx, `UPDATE memberships SET version=version+1,updated_at=$2 WHERE id=ANY($1)`, membershipIDs, now); err != nil {
			return OwnershipTransferResult{}, err
		}
		userIDs := []uuid.UUID{previousOwnerID}
		if targetUserID != previousOwnerID {
			userIDs = append(userIDs, targetUserID)
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET membership_version=membership_version+1,updated_at=$2 WHERE id=ANY($1)`, userIDs, now); err != nil {
			return OwnershipTransferResult{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE user_sessions SET membership_version=membership_version+1,last_seen_at=$2 WHERE user_id=ANY($1)`, userIDs, now); err != nil {
			return OwnershipTransferResult{}, err
		}
		for _, changed := range []struct {
			membershipID uuid.UUID
			userID       uuid.UUID
		}{{previousMembershipID, previousOwnerID}, {targetMembershipID, targetUserID}} {
			if changed.membershipID == previousMembershipID && changed.userID == previousOwnerID && previousMembershipID == targetMembershipID {
				continue
			}
			var membershipVersion int64
			if err := tx.QueryRow(ctx, `SELECT version FROM memberships WHERE id=$1`, changed.membershipID).Scan(&membershipVersion); err != nil {
				return OwnershipTransferResult{}, err
			}
			if err := appendOutbox(ctx, tx, "account.membership.changed.v1", changed.membershipID, membershipVersion, map[string]any{"organization_id": organizationID, "user_id": changed.userID, "membership_status": "ACTIVE", "membership_version": membershipVersion}); err != nil {
				return OwnershipTransferResult{}, err
			}
		}
		if platformAdmin {
			actor := &actorID
			if err := appendAdminAudit(ctx, tx, actor, principal, &organizationID, "admin.organization.ownership.transfer", "organization", organizationID.String(), map[string]any{"previous_owner_user_id": previousOwnerID, "new_owner_user_id": targetUserID, "idempotency_key": idempotencyKey}); err != nil {
				return OwnershipTransferResult{}, err
			}
		} else if err := appendAudit(ctx, tx, actorID, principal.SessionID, &organizationID, "organization.ownership.transfer", "organization", organizationID.String(), "SUCCEEDED", map[string]any{"previous_owner_user_id": previousOwnerID, "new_owner_user_id": targetUserID, "idempotency_key": idempotencyKey}); err != nil {
			return OwnershipTransferResult{}, err
		}
	}
	member, err := getMember(ctx, tx, targetMembershipID)
	if err != nil {
		return OwnershipTransferResult{}, err
	}
	result := OwnershipTransferResult{OrganizationID: organizationID, PreviousOwnerID: previousOwnerID, NewOwnerID: targetUserID, NewOwnerMembership: member, Replayed: !changed}
	stored := result
	stored.Replayed = false
	if err := saveMembershipCommand(ctx, tx, actorID, organizationID, targetUserID, targetMembershipID, "TRANSFER_OWNERSHIP", idempotencyKey, requestHash, stored, now); err != nil {
		return OwnershipTransferResult{}, err
	}
	return result, tx.Commit(ctx)
}

// Private durable receipt scope is never part of the returned HTTP member DTO.
// The existing command row binds it to actor, organization, key and request hash.
type membershipRemovalReceipt struct {
	MembershipMutationResult
	AffectedUnitIDs []uuid.UUID `json:"affected_unit_ids,omitempty"`
}
