package account

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/text/unicode/norm"
	"strings"
	"time"
)

func (r *PostgresRepository) TokenOwnerCandidates(ctx context.Context, actor TokenOwnerCandidateActor, filter TokenOwnerCandidateFilter) (TokenOwnerCandidatePage, error) {
	if filter.OwnerType != "" && filter.OwnerType != "USER" || filter.OrganizationID != nil {
		return TokenOwnerCandidatePage{}, ErrInvalidTokenOwnerCandidateFilter
	}
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
	// First acquire the authorization-version row lock in its own statement.
	// A blocked statement's role subqueries retain its initial snapshot, so
	// current permission facts must be read by a subsequent statement.
	var authorizationVersion int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT version FROM authorization_versions
WHERE user_id=$1 AND scope='PLATFORM' AND scope_id='00000000-0000-0000-0000-000000000000'::uuid
FOR SHARE),1)`, actorID).Scan(&authorizationVersion); err != nil {
		return TokenOwnerCandidatePage{}, err
	}
	// Evaluate current authority and cursor lifetime only after both replica locks.
	var databaseNow time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&databaseNow); err != nil {
		return TokenOwnerCandidatePage{}, err
	}
	databaseNow = databaseNow.UTC()
	var allowed bool
	err = tx.QueryRow(ctx, `
SELECT EXISTS(
         SELECT 1 FROM role_assignments assignment
         JOIN roles role_definition ON role_definition.code=assignment.role_code AND role_definition.scope='PLATFORM'
         JOIN role_permissions permission ON permission.role_code=assignment.role_code AND permission.permission_code=$2
         WHERE assignment.user_id=$1 AND assignment.organization_id IS NULL AND assignment.unit_id IS NULL
           AND assignment.revoked_at IS NULL AND assignment.starts_at<=$5
           AND (assignment.expires_at IS NULL OR assignment.expires_at>$5)
       )
FROM identities identity_record
JOIN users account_user ON account_user.id=identity_record.user_id AND account_user.status='ACTIVE'
WHERE account_user.id=$1 AND identity_record.issuer=$3 AND identity_record.subject=$4`, actorID, PermissionPlatformTokenOwnerCandidatesRead, actor.Issuer, actor.Subject, databaseNow).Scan(&allowed)
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenOwnerCandidatePage{}, ErrForbidden
	}
	if err != nil {
		return TokenOwnerCandidatePage{}, err
	}
	if !allowed {
		return TokenOwnerCandidatePage{}, ErrForbidden
	}
	if err := validateTokenOwnerCandidateCursorAuthorization(filter, authorizationVersion); err != nil {
		return TokenOwnerCandidatePage{}, err
	}

	if _, err := tx.Exec(ctx, `DELETE FROM token_owner_candidate_query_attempts WHERE expires_at<$3::timestamptz-interval '5 minutes' AND (actor_user_id=$1 OR caller_service=$2)`, actorID, actor.CallerService, databaseNow); err != nil {
		return TokenOwnerCandidatePage{}, err
	}
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
  AND high_water=$7 AND result='STARTED' AND completed_at IS NULL AND expires_at>$8
FOR SHARE`, scanID, actorID, actor.CallerService, actor.CallerSubject, authorizationVersion, filterFingerprint, filter.Cursor.HighWater, databaseNow).Scan(&highWater, &issuedAt, &expiresAt)
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
  (SELECT count(*)>=30 FROM token_owner_candidate_query_attempts WHERE actor_user_id=$1 AND occurred_at>$3::timestamptz-interval '1 minute')
  OR (SELECT count(*)>=60 FROM token_owner_candidate_query_attempts WHERE caller_service=$2 AND occurred_at>$3::timestamptz-interval '1 minute')
  OR (SELECT count(*)>=3 FROM token_owner_candidate_query_attempts WHERE actor_user_id=$1 AND completed_at IS NULL AND expires_at>$3)
  OR (SELECT count(*)>=10 FROM token_owner_candidate_query_attempts WHERE caller_service=$2 AND completed_at IS NULL AND expires_at>$3)`, actorID, actor.CallerService, databaseNow).Scan(&limited)
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
SELECT candidate.owner_id,candidate.owner_type,candidate.display_label,candidate.status,candidate.owner_version
FROM (
  SELECT account_user.id AS owner_id,'USER'::text AS owner_type,
         COALESCE(NULLIF(btrim(account_user.display_name),''),NULLIF(btrim(account_user.username),''),account_user.user_code) AS display_label,
         account_user.status,account_user.version AS owner_version,account_user.created_at
  FROM users account_user
  WHERE account_user.status IN ('ACTIVE','DISABLED','PROVISIONING')
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
		if err := rows.Scan(&item.ID, &item.Type, &item.DisplayLabel, &item.Status, &item.OwnerVersion); err != nil {
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
		OwnerVersions:        make(map[string]int64, len(items)),
	}
	for _, item := range items {
		page.OwnerVersions[item.ID.String()] = item.OwnerVersion
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
