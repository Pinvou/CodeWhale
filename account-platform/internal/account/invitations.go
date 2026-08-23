package account

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func normalizeInvitationTarget(targetType, value string) (string, string, []byte, error) {
	targetType = strings.ToUpper(strings.TrimSpace(targetType))
	value = strings.TrimSpace(value)
	switch targetType {
	case "PHONE":
		normalized, err := NormalizeMainlandMobile(value)
		if err != nil {
			return "", "", nil, err
		}
		return targetType, normalized[:3] + "****" + normalized[len(normalized)-4:], invitationTargetDigest(normalized), nil
	case "USERNAME":
		normalized, err := NormalizeRegistrationUsername(value)
		if err != nil {
			return "", "", nil, err
		}
		hint := normalized[:1] + "***"
		return targetType, hint, invitationTargetDigest(normalized), nil
	case "EMAIL":
		normalized := normalizeEmail(value)
		at := strings.LastIndex(normalized, "@")
		if at < 1 || at == len(normalized)-1 {
			return "", "", nil, ErrConflict
		}
		return targetType, normalized[:1] + "***" + normalized[at:], invitationTargetDigest(normalized), nil
	case "LINK":
		if value != "" {
			return "", "", nil, ErrConflict
		}
		return targetType, "", nil, nil
	default:
		return "", "", nil, ErrConflict
	}
}

func invitationTargetDigest(value string) []byte {
	digest := sha256.Sum256([]byte(value))
	return digest[:]
}

func invitationRequestHash(targetType string, targetDigest []byte, roles []string, expiresInHours int) string {
	canonical, _ := json.Marshal(map[string]any{"version": 1, "target_type": targetType, "target_digest": hex.EncodeToString(targetDigest), "roles": roles, "expires_in_hours": expiresInHours})
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:])
}

func scanInvitation(row pgx.Row) (Invitation, []byte, []byte, error) {
	var invitation Invitation
	var rolesJSON, targetDigest []byte
	err := row.Scan(&invitation.ID, &invitation.OrganizationID, &invitation.TargetType, &targetDigest, &invitation.TargetHint, &rolesJSON, &invitation.Status, &invitation.Version, &invitation.ExpiresAt, &invitation.AcceptedAt, &invitation.RevokedAt)
	if err == nil {
		err = json.Unmarshal(rolesJSON, &invitation.Roles)
	}
	return invitation, targetDigest, rolesJSON, err
}

const invitationSelect = `SELECT id,organization_id,target_type,target_digest,COALESCE(target_hint,''),roles,status,version,expires_at,accepted_at,revoked_at FROM invitations`

func (r *PostgresRepository) CreateInvitation(ctx context.Context, principal identity.Principal, organizationID uuid.UUID, request InvitationCreateRequest) (Invitation, error) {
	roles, err := validateInvitationRoles(request.Roles)
	if err != nil || request.ExpiresInHours < 1 || request.ExpiresInHours > 24*30 || !validOrganizationUnitIdempotencyKey(request.IdempotencyKey) {
		return Invitation{}, ErrConflict
	}
	roles = sortedInvitationRoles(roles)
	targetType, targetHint, targetDigest, err := normalizeInvitationTarget(request.TargetType, request.Target)
	if err != nil {
		return Invitation{}, ErrConflict
	}
	requestHash := invitationRequestHash(targetType, targetDigest, roles, request.ExpiresInHours)
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Invitation{}, err
	}
	defer tx.Rollback(ctx)
	actorID, err := activeUserID(ctx, tx, principal)
	if err != nil {
		return Invitation{}, err
	}
	if err := requireOrganizationPermission(ctx, tx, actorID, organizationID, nil, PermissionOrganizationMembersManage); err != nil {
		return Invitation{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "invitation:"+actorID.String()+":"+organizationID.String()+":"+request.IdempotencyKey); err != nil {
		return Invitation{}, err
	}
	var storedHash string
	invitation, _, _, lookupErr := scanInvitation(tx.QueryRow(ctx, invitationSelect+` WHERE invited_by=$1 AND organization_id=$2 AND idempotency_key=$3`, actorID, organizationID, strings.TrimSpace(request.IdempotencyKey)))
	if lookupErr == nil {
		if err := tx.QueryRow(ctx, `SELECT request_hash FROM invitations WHERE id=$1`, invitation.ID).Scan(&storedHash); err != nil {
			return Invitation{}, err
		}
		if storedHash != requestHash {
			return Invitation{}, ErrConflict
		}
		invitation.Replayed = true
		return invitation, tx.Commit(ctx)
	}
	if !errors.Is(lookupErr, pgx.ErrNoRows) {
		return Invitation{}, lookupErr
	}
	var organizationStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM organizations WHERE id=$1 FOR UPDATE`, organizationID).Scan(&organizationStatus); err != nil {
		return Invitation{}, err
	}
	if organizationStatus != "ACTIVE" {
		return Invitation{}, ErrConflict
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return Invitation{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	tokenHash := sha256.Sum256([]byte(token))
	now := r.now()
	invitation = Invitation{ID: uuid.Must(uuid.NewV7()), OrganizationID: organizationID, TargetType: targetType, TargetHint: targetHint, Roles: roles, Status: "PENDING", Version: 1, ExpiresAt: now.Add(time.Duration(request.ExpiresInHours) * time.Hour), Token: token}
	rolesJSON, _ := json.Marshal(roles)
	var legacyEmail *string
	if targetType == "EMAIL" {
		normalized := normalizeEmail(request.Target)
		legacyEmail = &normalized
	}
	if _, err := tx.Exec(ctx, `INSERT INTO invitations(id,organization_id,email_normalized,token_hash,status,roles,invited_by,expires_at,created_at,updated_at,target_type,target_digest,target_hint,idempotency_key,request_hash) VALUES($1,$2,$3,$4,'PENDING',$5,$6,$7,$8,$8,$9,$10,$11,$12,$13)`, invitation.ID, organizationID, legacyEmail, tokenHash[:], rolesJSON, actorID, invitation.ExpiresAt, now, targetType, targetDigest, targetHint, strings.TrimSpace(request.IdempotencyKey), requestHash); err != nil {
		return Invitation{}, mapConstraintError(err)
	}
	if err := appendAudit(ctx, tx, actorID, principal.SessionID, &organizationID, "invitation.create", "invitation", invitation.ID.String(), "SUCCEEDED", map[string]any{"target_type": targetType, "target_hint": targetHint, "roles": roles, "expires_at": invitation.ExpiresAt, "idempotency_key": request.IdempotencyKey}); err != nil {
		return Invitation{}, err
	}
	return invitation, tx.Commit(ctx)
}

func (r *PostgresRepository) ListMyInvitations(ctx context.Context, principal identity.Principal) ([]Invitation, error) {
	var userID uuid.UUID
	var username, mobile, email *string
	if err := r.pool.QueryRow(ctx, `SELECT u.id,u.username,CASE WHEN u.mobile_verified_at IS NOT NULL THEN u.mobile_e164 END,u.email FROM identities identity JOIN users u ON u.id=identity.user_id WHERE identity.issuer=$1 AND identity.subject=$2 AND u.status='ACTIVE'`, principal.Issuer, principal.Subject).Scan(&userID, &username, &mobile, &email); err != nil {
		return nil, err
	}
	digests := map[string][]byte{}
	if username != nil {
		digests["USERNAME"] = invitationTargetDigest(strings.ToLower(strings.TrimSpace(*username)))
	}
	if mobile != nil {
		digests["PHONE"] = invitationTargetDigest(strings.TrimSpace(*mobile))
	}
	if email != nil {
		digests["EMAIL"] = invitationTargetDigest(normalizeEmail(*email))
	}
	rows, err := r.pool.Query(ctx, invitationSelect+` WHERE status='PENDING' AND expires_at>now() AND ((target_type='USERNAME' AND target_digest=$1) OR (target_type='PHONE' AND target_digest=$2) OR (target_type='EMAIL' AND target_digest=$3)) ORDER BY expires_at,id`, digests["USERNAME"], digests["PHONE"], digests["EMAIL"])
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Invitation, 0)
	for rows.Next() {
		invitation, _, _, err := scanInvitation(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, invitation)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) RevokeInvitation(ctx context.Context, principal identity.Principal, organizationID, invitationID uuid.UUID) (Invitation, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Invitation{}, err
	}
	defer tx.Rollback(ctx)
	actorID, err := activeUserID(ctx, tx, principal)
	if err != nil {
		return Invitation{}, err
	}
	if err := requireOrganizationPermission(ctx, tx, actorID, organizationID, nil, PermissionOrganizationMembersManage); err != nil {
		return Invitation{}, err
	}
	invitation, _, _, err := scanInvitation(tx.QueryRow(ctx, invitationSelect+` WHERE id=$1 AND organization_id=$2 FOR UPDATE`, invitationID, organizationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Invitation{}, ErrNotFound
	}
	if err != nil {
		return Invitation{}, err
	}
	if invitation.Status == "REVOKED" {
		invitation.Replayed = true
		return invitation, tx.Commit(ctx)
	}
	if invitation.Status != "PENDING" {
		return Invitation{}, ErrConflict
	}
	now := r.now()
	if _, err := tx.Exec(ctx, `UPDATE invitations SET status='REVOKED',revoked_at=$2,version=version+1,updated_at=$2 WHERE id=$1`, invitationID, now); err != nil {
		return Invitation{}, err
	}
	invitation.Status, invitation.RevokedAt, invitation.Version = "REVOKED", &now, invitation.Version+1
	if err := appendAudit(ctx, tx, actorID, principal.SessionID, &organizationID, "invitation.revoke", "invitation", invitationID.String(), "SUCCEEDED", nil); err != nil {
		return Invitation{}, err
	}
	return invitation, tx.Commit(ctx)
}

func (r *PostgresRepository) AcceptInvitation(ctx context.Context, principal identity.Principal, token string, invitationID *uuid.UUID) (MembershipMutationResult, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return MembershipMutationResult{}, err
	}
	defer tx.Rollback(ctx)
	userID, err := activeUserID(ctx, tx, principal)
	if err != nil {
		return MembershipMutationResult{}, err
	}
	var username, mobile, email *string
	if err := tx.QueryRow(ctx, `SELECT username,CASE WHEN mobile_verified_at IS NOT NULL THEN mobile_e164 END,email FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&username, &mobile, &email); err != nil {
		return MembershipMutationResult{}, err
	}
	query, args := invitationSelect+` WHERE token_hash=$1 FOR UPDATE`, []any{invitationTokenHash(token)}
	if invitationID != nil {
		query, args = invitationSelect+` WHERE id=$1 AND target_type<>'LINK' FOR UPDATE`, []any{*invitationID}
	}
	invitation, targetDigest, _, err := scanInvitation(tx.QueryRow(ctx, query, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return MembershipMutationResult{}, ErrNotFound
	}
	if err != nil {
		return MembershipMutationResult{}, err
	}
	if !invitationTargetMatches(invitation.TargetType, targetDigest, username, mobile, email) {
		return MembershipMutationResult{}, ErrNotFound
	}
	if invitation.Status == "ACCEPTED" {
		var acceptedBy uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT accepted_by FROM invitations WHERE id=$1`, invitation.ID).Scan(&acceptedBy); err != nil || acceptedBy != userID {
			return MembershipMutationResult{}, ErrNotFound
		}
		var membershipID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM memberships WHERE organization_id=$1 AND user_id=$2`, invitation.OrganizationID, userID).Scan(&membershipID); err != nil {
			return MembershipMutationResult{}, err
		}
		member, err := getMember(ctx, tx, membershipID)
		if err != nil {
			return MembershipMutationResult{}, err
		}
		return MembershipMutationResult{OrganizationID: invitation.OrganizationID, Member: member, Replayed: true}, tx.Commit(ctx)
	}
	if invitation.Status == "EXPIRED" {
		return MembershipMutationResult{}, ErrInvitationExpired
	}
	if invitation.Status != "PENDING" {
		return MembershipMutationResult{}, ErrNotFound
	}
	if !invitation.ExpiresAt.After(r.now()) {
		now := r.now()
		if _, err := tx.Exec(ctx, `UPDATE invitations SET status='EXPIRED',version=version+1,updated_at=$2 WHERE id=$1`, invitation.ID, now); err != nil {
			return MembershipMutationResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return MembershipMutationResult{}, err
		}
		return MembershipMutationResult{}, ErrInvitationExpired
	}
	var organizationStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM organizations WHERE id=$1 FOR UPDATE`, invitation.OrganizationID).Scan(&organizationStatus); err != nil {
		return MembershipMutationResult{}, err
	}
	if organizationStatus != "ACTIVE" {
		return MembershipMutationResult{}, ErrConflict
	}
	now := r.now()
	var membershipID uuid.UUID
	var membershipStatus string
	var membershipVersion int64
	err = tx.QueryRow(ctx, `SELECT id,status,version FROM memberships WHERE organization_id=$1 AND user_id=$2 FOR UPDATE`, invitation.OrganizationID, userID).Scan(&membershipID, &membershipStatus, &membershipVersion)
	membershipChanged := false
	if errors.Is(err, pgx.ErrNoRows) {
		membershipID, membershipVersion, membershipStatus, membershipChanged = uuid.Must(uuid.NewV7()), 1, "ACTIVE", true
		if _, err := tx.Exec(ctx, `INSERT INTO memberships(id,organization_id,user_id,status,joined_at,updated_at) VALUES($1,$2,$3,'ACTIVE',$4,$4)`, membershipID, invitation.OrganizationID, userID, now); err != nil {
			return MembershipMutationResult{}, mapConstraintError(err)
		}
	} else if err != nil {
		return MembershipMutationResult{}, err
	} else if membershipStatus != "ACTIVE" {
		membershipChanged = true
		membershipVersion++
		if _, err := tx.Exec(ctx, `UPDATE memberships SET status='ACTIVE',version=$2,updated_at=$3 WHERE id=$1`, membershipID, membershipVersion, now); err != nil {
			return MembershipMutationResult{}, err
		}
	}
	if membershipChanged {
		for _, role := range invitation.Roles {
			if _, err := tx.Exec(ctx, `INSERT INTO role_bindings(id,membership_id,role,created_by,created_at,starts_at,grant_source) VALUES($1,$2,$3,$4,$5,$5,'INVITATION')`, uuid.Must(uuid.NewV7()), membershipID, role, userID, now); err != nil {
				return MembershipMutationResult{}, mapConstraintError(err)
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET membership_version=membership_version+1,updated_at=$2 WHERE id=$1`, userID, now); err != nil {
			return MembershipMutationResult{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE user_sessions SET membership_version=membership_version+1,last_seen_at=$2 WHERE user_id=$1`, userID, now); err != nil {
			return MembershipMutationResult{}, err
		}
		if err := appendOutbox(ctx, tx, "account.membership.changed.v1", membershipID, membershipVersion, map[string]any{"organization_id": invitation.OrganizationID, "user_id": userID, "membership_status": "ACTIVE", "membership_version": membershipVersion}); err != nil {
			return MembershipMutationResult{}, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE invitations SET status='ACCEPTED',accepted_by=$2,accepted_at=$3,version=version+1,updated_at=$3 WHERE id=$1`, invitation.ID, userID, now); err != nil {
		return MembershipMutationResult{}, err
	}
	if err := appendAudit(ctx, tx, userID, principal.SessionID, &invitation.OrganizationID, "invitation.accept", "invitation", invitation.ID.String(), "SUCCEEDED", map[string]any{"membership_id": membershipID, "target_type": invitation.TargetType}); err != nil {
		return MembershipMutationResult{}, err
	}
	member, err := getMember(ctx, tx, membershipID)
	if err != nil {
		return MembershipMutationResult{}, err
	}
	return MembershipMutationResult{OrganizationID: invitation.OrganizationID, Member: member, Replayed: !membershipChanged}, tx.Commit(ctx)
}

func invitationTokenHash(token string) []byte {
	digest := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return digest[:]
}

func invitationTargetMatches(targetType string, expected []byte, username, mobile, email *string) bool {
	if targetType == "LINK" {
		return true
	}
	var value string
	switch targetType {
	case "USERNAME":
		if username != nil {
			value = strings.ToLower(strings.TrimSpace(*username))
		}
	case "PHONE":
		if mobile != nil {
			value = strings.TrimSpace(*mobile)
		}
	case "EMAIL":
		if email != nil {
			value = normalizeEmail(*email)
		}
	}
	return value != "" && bytes.Equal(expected, invitationTargetDigest(value))
}

func sortedInvitationRoles(roles []string) []string {
	result := append([]string(nil), roles...)
	sort.Strings(result)
	return result
}
