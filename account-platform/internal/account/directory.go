package account

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/text/unicode/norm"
)

func (r *PostgresRepository) TokenOwnerCandidates(ctx context.Context, actor TokenOwnerCandidateActor, filter TokenOwnerCandidateFilter) (TokenOwnerCandidatePage, error) {
	if strings.TrimSpace(actor.Issuer) == "" || strings.TrimSpace(actor.Subject) == "" || strings.TrimSpace(actor.CallerService) == "" || strings.TrimSpace(actor.CallerSubject) == "" || len(actor.CallerService) > 128 || len(actor.CallerSubject) > 512 || filter.Limit < 1 || filter.Limit > 50000 {
		return TokenOwnerCandidatePage{}, ErrInvalidTokenOwnerCandidateFilter
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadWrite})
	if err != nil {
		return TokenOwnerCandidatePage{}, err
	}
	defer tx.Rollback(ctx)

	var actorID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT account_user.id
FROM identities identity_record
JOIN users account_user ON account_user.id=identity_record.user_id AND account_user.status='ACTIVE'
WHERE identity_record.issuer=$1 AND identity_record.subject=$2`, actor.Issuer, actor.Subject).Scan(&actorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenOwnerCandidatePage{}, ErrForbidden
	}
	if err != nil {
		return TokenOwnerCandidatePage{}, err
	}
	// Locks are keyed only by trusted workload identity and the resolved human
	// actor. They serialize counters across replicas and never consume proxy or
	// client supplied address headers.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('token-owner-actor:'||$1::text,0))`, actorID); err != nil {
		return TokenOwnerCandidatePage{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('token-owner-service:'||$1,0))`, actor.CallerService); err != nil {
		return TokenOwnerCandidatePage{}, err
	}
	var authorizationVersion int64
	var allowed bool
	err = tx.QueryRow(ctx, `
SELECT COALESCE((
         SELECT version FROM authorization_versions
         WHERE user_id=$1 AND scope='PLATFORM' AND scope_id='00000000-0000-0000-0000-000000000000'::uuid
         FOR SHARE
       ),1),
       EXISTS(
         SELECT 1 FROM role_assignments assignment
         JOIN roles role_definition ON role_definition.code=assignment.role_code AND role_definition.scope='PLATFORM'
         JOIN role_permissions permission ON permission.role_code=assignment.role_code AND permission.permission_code=$2
         WHERE assignment.user_id=$1 AND assignment.organization_id IS NULL AND assignment.unit_id IS NULL
           AND assignment.revoked_at IS NULL AND assignment.starts_at<=now()
           AND (assignment.expires_at IS NULL OR assignment.expires_at>now())
       )`, actorID, PermissionPlatformTokenOwnerCandidatesRead).Scan(&authorizationVersion, &allowed)
	if err != nil {
		return TokenOwnerCandidatePage{}, err
	}
	if !allowed {
		return TokenOwnerCandidatePage{}, ErrForbidden
	}
	if err := validateTokenOwnerCandidateCursorAuthorization(filter, authorizationVersion); err != nil {
		return TokenOwnerCandidatePage{}, err
	}

	if _, err := tx.Exec(ctx, `DELETE FROM token_owner_candidate_query_attempts WHERE expires_at<now()-interval '5 minutes' AND (actor_user_id=$1 OR caller_service=$2)`, actorID, actor.CallerService); err != nil {
		return TokenOwnerCandidatePage{}, err
	}
	var databaseNow time.Time
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&databaseNow); err != nil {
		return TokenOwnerCandidatePage{}, err
	}
	databaseNow = databaseNow.UTC()
	filterFingerprint := tokenOwnerCandidateFilterFingerprint(filter)
	scanID := uuid.Must(uuid.NewV7())
	highWater, issuedAt, expiresAt := databaseNow, databaseNow, databaseNow.Add(tokenOwnerCandidateCursorTTL)
	cursorType, cursorID := "", uuid.Nil
	if filter.Cursor != nil {
		scanID = filter.Cursor.ScanID
		err := tx.QueryRow(ctx, `SELECT high_water,occurred_at,expires_at
FROM token_owner_candidate_query_attempts
WHERE id=$1 AND actor_user_id=$2 AND caller_service=$3 AND caller_subject=$4
  AND authorization_version=$5 AND filter_fingerprint=$6
  AND high_water=$7 AND result='STARTED' AND completed_at IS NULL AND expires_at>now()
FOR SHARE`, scanID, actorID, actor.CallerService, actor.CallerSubject, authorizationVersion, filterFingerprint, filter.Cursor.HighWater).Scan(&highWater, &issuedAt, &expiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return TokenOwnerCandidatePage{}, ErrTokenOwnerCandidateCursorExpired
		}
		if err != nil {
			return TokenOwnerCandidatePage{}, err
		}
		cursorType, cursorID = filter.Cursor.OwnerType, filter.Cursor.OwnerID
	} else {
		var limited bool
		err = tx.QueryRow(ctx, `
SELECT
  (SELECT count(*)>=30 FROM token_owner_candidate_query_attempts WHERE actor_user_id=$1 AND occurred_at>now()-interval '1 minute')
  OR (SELECT count(*)>=60 FROM token_owner_candidate_query_attempts WHERE caller_service=$2 AND occurred_at>now()-interval '1 minute')
  OR (SELECT count(*)>=3 FROM token_owner_candidate_query_attempts WHERE actor_user_id=$1 AND completed_at IS NULL AND expires_at>now())
  OR (SELECT count(*)>=10 FROM token_owner_candidate_query_attempts WHERE caller_service=$2 AND completed_at IS NULL AND expires_at>now())`, actorID, actor.CallerService).Scan(&limited)
		if err != nil {
			return TokenOwnerCandidatePage{}, err
		}
		result := "STARTED"
		completedAt := (*time.Time)(nil)
		returnErr := filter.CursorErr
		if limited {
			result, completedAt, returnErr = "RATE_LIMITED", &databaseNow, ErrTokenOwnerCandidateRateLimited
		} else if returnErr != nil {
			result, completedAt = "INVALID_CURSOR", &databaseNow
		}
		if err := recordTokenOwnerCandidateScan(ctx, tx, scanID, actorID, actor, authorizationVersion, filterFingerprint, highWater, issuedAt, expiresAt, result, completedAt); err != nil {
			return TokenOwnerCandidatePage{}, err
		}
		if returnErr != nil {
			if err := tx.Commit(ctx); err != nil {
				return TokenOwnerCandidatePage{}, err
			}
			return TokenOwnerCandidatePage{}, returnErr
		}
	}
	rows, err := tx.Query(ctx, `
SELECT candidate.owner_id,candidate.owner_type,candidate.display_label,candidate.status
FROM (
  SELECT account_user.id AS owner_id,'USER'::text AS owner_type,
         COALESCE(NULLIF(btrim(account_user.display_name),''),NULLIF(btrim(account_user.username),''),account_user.user_code) AS display_label,
         account_user.status,account_user.created_at
  FROM users account_user
  WHERE account_user.status<>'DELETED'
  UNION ALL
  SELECT organization.id,'ORGANIZATION',organization.name,organization.status,organization.created_at
  FROM organizations organization
  WHERE organization.status<>'DELETED'
  UNION ALL
  SELECT unit.id,'UNIT',unit.name,unit.status,unit.created_at
  FROM organization_units unit
  JOIN organizations organization ON organization.id=unit.organization_id AND organization.status<>'DELETED'
  WHERE unit.status<>'DELETED'
) candidate
WHERE ($1='' OR candidate.owner_type=$1)
  AND candidate.created_at<=$2
  AND ($3='' OR (candidate.owner_type,candidate.owner_id)>($3,$4))
ORDER BY candidate.owner_type,candidate.owner_id
LIMIT $5`, filter.OwnerType, highWater, cursorType, cursorID, filter.Limit+1)
	if err != nil {
		return TokenOwnerCandidatePage{}, err
	}
	items := make([]TokenOwnerCandidate, 0, filter.Limit+1)
	for rows.Next() {
		var item TokenOwnerCandidate
		if err := rows.Scan(&item.ID, &item.Type, &item.DisplayLabel, &item.Status); err != nil {
			rows.Close()
			return TokenOwnerCandidatePage{}, err
		}
		item.DisplayLabel = norm.NFKC.String(strings.TrimSpace(item.DisplayLabel))
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return TokenOwnerCandidatePage{}, err
	}
	rows.Close()
	hasMore := len(items) > filter.Limit
	if hasMore {
		items = items[:filter.Limit]
	}
	page := TokenOwnerCandidatePage{
		Items:                items,
		HasMore:              hasMore,
		Limit:                filter.Limit,
		AuthorizationVersion: authorizationVersion,
	}
	if page.HasMore && len(page.Items) > 0 {
		last := page.Items[len(page.Items)-1]
		page.next = &tokenOwnerCandidateCursor{
			AuthorizationVersion: authorizationVersion,
			ScanID:               scanID,
			HighWater:            highWater,
			OwnerType:            last.Type,
			OwnerID:              last.ID,
			IssuedAt:             issuedAt,
			ExpiresAt:            expiresAt,
		}
	} else {
		if _, err := tx.Exec(ctx, `UPDATE token_owner_candidate_query_attempts
SET result='COMPLETED',completed_at=clock_timestamp()
WHERE id=$1 AND result='STARTED' AND completed_at IS NULL`, scanID); err != nil {
			return TokenOwnerCandidatePage{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return TokenOwnerCandidatePage{}, err
	}
	return page, nil
}

func validateTokenOwnerCandidateCursorAuthorization(filter TokenOwnerCandidateFilter, authorizationVersion int64) error {
	if filter.Cursor != nil && filter.Cursor.AuthorizationVersion != authorizationVersion {
		return ErrTokenOwnerCandidateCursorExpired
	}
	return nil
}

func recordTokenOwnerCandidateScan(ctx context.Context, tx pgx.Tx, scanID, actorID uuid.UUID, actor TokenOwnerCandidateActor, authorizationVersion int64, filterFingerprint string, highWater, issuedAt, expiresAt time.Time, result string, completedAt *time.Time) error {
	_, err := tx.Exec(ctx, `
INSERT INTO token_owner_candidate_query_attempts(
  id,actor_user_id,caller_service,caller_subject,authorization_version,
  filter_fingerprint,high_water,result,occurred_at,expires_at,completed_at
) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, scanID, actorID, actor.CallerService, actor.CallerSubject, authorizationVersion, filterFingerprint, highWater, result, issuedAt, expiresAt, completedAt)
	return err
}

func (r *PostgresRepository) ListUnits(ctx context.Context, principal identity.Principal, organizationID uuid.UUID, rootUnitID *uuid.UUID) (UnitTree, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return UnitTree{}, err
	}
	defer tx.Rollback(ctx)
	actorID, err := activeUserID(ctx, tx, principal)
	if err != nil {
		return UnitTree{}, err
	}
	if err := requireOrganizationPermission(ctx, tx, actorID, organizationID, rootUnitID, PermissionOrganizationUnitsRead); err != nil {
		return UnitTree{}, err
	}
	var structureVersion int64
	if err := tx.QueryRow(ctx, `SELECT structure_version FROM organizations WHERE id=$1 AND status='ACTIVE'`, organizationID).Scan(&structureVersion); errors.Is(err, pgx.ErrNoRows) {
		return UnitTree{}, ErrNotFound
	} else if err != nil {
		return UnitTree{}, err
	}
	rows, err := tx.Query(ctx, `SELECT unit.id,unit.organization_id,unit.parent_id,unit.name,unit.status,unit.version
FROM organization_units unit
WHERE unit.organization_id=$1 AND unit.status='ACTIVE'
  AND ($2::uuid IS NULL OR EXISTS(
    SELECT 1 FROM organization_unit_closure closure
    WHERE closure.organization_id=$1 AND closure.ancestor_id=$2 AND closure.descendant_id=unit.id
  ))
ORDER BY unit.parent_id NULLS FIRST,unit.name,unit.id`, organizationID, rootUnitID)
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
	if err := rows.Err(); err != nil {
		return UnitTree{}, err
	}
	return UnitTree{Items: items, StructureVersion: structureVersion}, tx.Commit(ctx)
}

func (r *PostgresRepository) ListMembers(ctx context.Context, principal identity.Principal, organizationID uuid.UUID, unitID *uuid.UUID) ([]Member, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	actorID, err := activeUserID(ctx, tx, principal)
	if err != nil {
		return nil, err
	}
	if err := requireOrganizationPermission(ctx, tx, actorID, organizationID, unitID, PermissionOrganizationMembersRead); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
SELECT m.id,u.id,u.user_code,u.display_name,u.email,m.status,m.version,m.joined_at,
       ARRAY(SELECT DISTINCT rb.role FROM role_bindings rb WHERE rb.membership_id=m.id AND rb.revoked_at IS NULL AND rb.starts_at<=now() AND (rb.expires_at IS NULL OR rb.expires_at>now()) ORDER BY rb.role),
       ARRAY(SELECT mu.unit_id FROM membership_units mu WHERE mu.membership_id=m.id ORDER BY mu.unit_id),
       ARRAY(SELECT rb.unit_id FROM role_bindings rb WHERE rb.membership_id=m.id AND rb.role='UNIT_ADMIN' AND rb.revoked_at IS NULL AND rb.starts_at<=now() AND (rb.expires_at IS NULL OR rb.expires_at>now()) ORDER BY rb.unit_id)
FROM memberships m JOIN users u ON u.id=m.user_id
WHERE m.organization_id=$1 AND m.status='ACTIVE'
  AND ($2::uuid IS NULL OR (
    EXISTS(
      SELECT 1 FROM membership_units scoped_membership
      JOIN organization_unit_closure scoped_closure ON scoped_closure.organization_id=m.organization_id AND scoped_closure.descendant_id=scoped_membership.unit_id
      WHERE scoped_membership.membership_id=m.id AND scoped_closure.ancestor_id=$2
    )
    AND NOT EXISTS(
      SELECT 1 FROM membership_units outside_membership
      WHERE outside_membership.membership_id=m.id AND NOT EXISTS(
        SELECT 1 FROM organization_unit_closure outside_closure
        WHERE outside_closure.organization_id=m.organization_id AND outside_closure.ancestor_id=$2 AND outside_closure.descendant_id=outside_membership.unit_id
      )
    )
  ))
ORDER BY COALESCE(u.display_name,u.email,u.id::text),u.id`, organizationID, unitID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Member, 0)
	for rows.Next() {
		var item Member
		if err := rows.Scan(&item.MembershipID, &item.UserID, &item.UserCode, &item.DisplayName, &item.Email, &item.Status, &item.Version, &item.JoinedAt, &item.Roles, &item.UnitIDs, &item.UnitAdminIDs); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, tx.Commit(ctx)
}

func (r *PostgresRepository) ListQuotaMembers(ctx context.Context, principal identity.Principal, organizationID uuid.UUID) ([]QuotaMember, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	actorID, err := activeUserID(ctx, tx, principal)
	if err != nil {
		return nil, err
	}
	if err := requireOrganizationPermission(ctx, tx, actorID, organizationID, nil, PermissionOrganizationQuotaAllocate); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT member.user_id,account_user.display_name,member.version
FROM memberships member JOIN users account_user ON account_user.id=member.user_id
WHERE member.organization_id=$1 AND member.status='ACTIVE' AND account_user.status='ACTIVE'
ORDER BY COALESCE(account_user.display_name,account_user.id::text),account_user.id`, organizationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]QuotaMember, 0)
	for rows.Next() {
		var item QuotaMember
		if err := rows.Scan(&item.UserID, &item.DisplayName, &item.Version); err != nil {
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
	if err := requireOrganizationMemberPermission(ctx, tx, actorID, organizationID, targetUserID, PermissionOrganizationRolesAssign); err != nil {
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
		if _, err := tx.Exec(ctx, `INSERT INTO role_bindings(id,membership_id,role,created_by,created_at,starts_at,grant_source) VALUES($1,$2,$3,$4,$5,$5,'ORGANIZATION_ADMIN')`, uuid.Must(uuid.NewV7()), membershipID, role, actorID, now); err != nil {
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
SELECT m.id,u.id,u.user_code,u.display_name,u.email,m.status,m.version,m.joined_at,
       ARRAY(SELECT DISTINCT rb.role FROM role_bindings rb WHERE rb.membership_id=m.id AND rb.revoked_at IS NULL AND rb.starts_at<=now() AND (rb.expires_at IS NULL OR rb.expires_at>now()) ORDER BY rb.role),
       ARRAY(SELECT mu.unit_id FROM membership_units mu WHERE mu.membership_id=m.id ORDER BY mu.unit_id),
       ARRAY(SELECT rb.unit_id FROM role_bindings rb WHERE rb.membership_id=m.id AND rb.role='UNIT_ADMIN' AND rb.revoked_at IS NULL AND rb.starts_at<=now() AND (rb.expires_at IS NULL OR rb.expires_at>now()) ORDER BY rb.unit_id)
FROM memberships m JOIN users u ON u.id=m.user_id WHERE m.id=$1`, membershipID).Scan(&item.MembershipID, &item.UserID, &item.UserCode, &item.DisplayName, &item.Email, &item.Status, &item.Version, &item.JoinedAt, &item.Roles, &item.UnitIDs, &item.UnitAdminIDs)
	return item, err
}
