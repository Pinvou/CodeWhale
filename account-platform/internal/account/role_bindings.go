package account

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type RoleBinding struct {
	ID             uuid.UUID  `json:"binding_id"`
	UserID         uuid.UUID  `json:"user_id"`
	RoleCode       string     `json:"role_code"`
	Scope          string     `json:"scope"`
	OrganizationID *uuid.UUID `json:"organization_id,omitempty"`
	UnitID         *uuid.UUID `json:"unit_id,omitempty"`
	StartsAt       time.Time  `json:"starts_at"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	Status         string     `json:"status"`
	GrantSource    string     `json:"grant_source"`
	CreatedBy      *uuid.UUID `json:"created_by,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	RevokedAt      *time.Time `json:"revoked_at,omitempty"`
	RevokedBy      *uuid.UUID `json:"revoked_by,omitempty"`
}

type RoleBindingGrant struct {
	UserID         uuid.UUID
	RoleCode       string
	OrganizationID *uuid.UUID
	UnitID         *uuid.UUID
	StartsAt       *time.Time
	ExpiresAt      *time.Time
	IdempotencyKey string
	Reason         string
}

const effectiveRoleAssignmentSQL = "ra.revoked_at IS NULL AND ra.starts_at <= now() AND (ra.expires_at IS NULL OR ra.expires_at > now())"

func (r *PostgresRepository) ListMyRoleBindings(ctx context.Context, principal identity.Principal) ([]RoleBinding, error) {
	var userID uuid.UUID
	if err := r.pool.QueryRow(ctx, `SELECT u.id FROM identities i JOIN users u ON u.id=i.user_id WHERE i.issuer=$1 AND i.subject=$2 AND u.status='ACTIVE'`, principal.Issuer, principal.Subject).Scan(&userID); errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrForbidden
	} else if err != nil {
		return nil, err
	}
	return r.queryRoleBindings(ctx, `ra.user_id=$1`, userID)
}

func (r *PostgresRepository) ListOrganizationRoleBindings(ctx context.Context, principal identity.Principal, organizationID uuid.UUID) ([]RoleBinding, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	actorID, err := activeUserID(ctx, tx, principal)
	if err != nil {
		return nil, err
	}
	if err := requireOrganizationPermission(ctx, tx, actorID, organizationID, nil, "org.roles.read"); err != nil {
		return nil, err
	}
	items, err := queryRoleBindings(ctx, tx, `ra.organization_id=$1`, organizationID)
	if err != nil {
		return nil, err
	}
	return items, tx.Commit(ctx)
}

func (r *PostgresRepository) queryRoleBindings(ctx context.Context, condition string, args ...any) ([]RoleBinding, error) {
	return queryRoleBindings(ctx, r.pool, condition, args...)
}

type roleBindingQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func queryRoleBindings(ctx context.Context, database roleBindingQuerier, condition string, args ...any) ([]RoleBinding, error) {
	rows, err := database.Query(ctx, `
SELECT ra.id,ra.user_id,ra.role_code,ro.scope,ra.organization_id,ra.unit_id,
       ra.starts_at,ra.expires_at,
       CASE WHEN ra.revoked_at IS NOT NULL AND ra.expires_at IS NOT NULL AND ra.revoked_at=ra.expires_at THEN 'EXPIRED'
            WHEN ra.revoked_at IS NOT NULL THEN 'REVOKED'
            WHEN ra.expires_at IS NOT NULL AND ra.expires_at<=now() THEN 'EXPIRED'
            WHEN ra.starts_at>now() THEN 'SCHEDULED' ELSE 'ACTIVE' END,
       ra.grant_source,ra.created_by,ra.created_at,ra.revoked_at,ra.revoked_by
FROM role_assignments ra JOIN roles ro ON ro.code=ra.role_code
WHERE `+condition+`
ORDER BY ro.scope,ra.organization_id NULLS FIRST,ra.user_id,ra.role_code,ra.unit_id NULLS FIRST,ra.starts_at,ra.id LIMIT 500`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]RoleBinding, 0)
	for rows.Next() {
		var item RoleBinding
		if err := rows.Scan(&item.ID, &item.UserID, &item.RoleCode, &item.Scope, &item.OrganizationID, &item.UnitID, &item.StartsAt, &item.ExpiresAt, &item.Status, &item.GrantSource, &item.CreatedBy, &item.CreatedAt, &item.RevokedAt, &item.RevokedBy); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func normalizeRoleBindingGrant(request RoleBindingGrant, now time.Time) (RoleBindingGrant, error) {
	// PostgreSQL timestamptz and pgx retain microseconds. Compare the same
	// durable precision on the first write and subsequent intent replay.
	now = now.UTC().Truncate(time.Microsecond)
	request.RoleCode = strings.ToUpper(strings.TrimSpace(request.RoleCode))
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	if request.UserID == uuid.Nil || request.OrganizationID == nil || *request.OrganizationID == uuid.Nil || request.RoleCode == "" || len(request.IdempotencyKey) < 8 || len(request.IdempotencyKey) > 120 {
		return RoleBindingGrant{}, ErrConflict
	}
	if request.StartsAt == nil {
		value := now
		request.StartsAt = &value
	} else {
		value := request.StartsAt.UTC().Truncate(time.Microsecond)
		request.StartsAt = &value
	}
	if request.ExpiresAt != nil {
		value := request.ExpiresAt.UTC().Truncate(time.Microsecond)
		request.ExpiresAt = &value
		if !value.After(*request.StartsAt) || !value.After(now) {
			return RoleBindingGrant{}, ErrConflict
		}
	}
	return request, nil
}

func (r *PostgresRepository) GrantOrganizationRoleBinding(ctx context.Context, principal identity.Principal, request RoleBindingGrant) (RoleBinding, error) {
	defaultStart := request.StartsAt == nil
	now := r.now()
	request, err := normalizeRoleBindingGrant(request, now)
	if err != nil {
		return RoleBinding{}, err
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return RoleBinding{}, err
	}
	defer tx.Rollback(ctx)
	actorID, err := activeUserID(ctx, tx, principal)
	if err != nil {
		return RoleBinding{}, err
	}
	var scope string
	var assignable bool
	if err := tx.QueryRow(ctx, `SELECT scope,assignable FROM roles WHERE code=$1`, request.RoleCode).Scan(&scope, &assignable); errors.Is(err, pgx.ErrNoRows) {
		return RoleBinding{}, ErrConflict
	} else if err != nil {
		return RoleBinding{}, err
	}
	if !assignable || scope == "PLATFORM" || scope == "PERSONAL" || request.RoleCode == "OWNER" || (scope == "UNIT") != (request.UnitID != nil) {
		return RoleBinding{}, ErrConflict
	}
	if request.UnitID != nil {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organization_units WHERE id=$1 AND organization_id=$2 AND status='ACTIVE')`, *request.UnitID, *request.OrganizationID).Scan(&exists); err != nil {
			return RoleBinding{}, err
		}
		if !exists {
			return RoleBinding{}, ErrNotFound
		}
	}
	if err := requireOrganizationPermission(ctx, tx, actorID, *request.OrganizationID, request.UnitID, PermissionOrganizationRolesAssign); err != nil {
		return RoleBinding{}, err
	}
	var membershipID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM memberships WHERE organization_id=$1 AND user_id=$2 AND status='ACTIVE' FOR UPDATE`, *request.OrganizationID, request.UserID).Scan(&membershipID); errors.Is(err, pgx.ErrNoRows) {
		return RoleBinding{}, ErrNotFound
	} else if err != nil {
		return RoleBinding{}, err
	}
	if existing, found, err := roleBindingByIdempotencyKey(ctx, tx, membershipID, request.IdempotencyKey); err != nil {
		return RoleBinding{}, err
	} else if found {
		// An omitted start denotes the original first grant time on replay.
		// Explicit facts and the actor must still identify that same intent.
		if existing.CreatedBy == nil || *existing.CreatedBy != actorID || existing.UserID != request.UserID || existing.RoleCode != request.RoleCode || !uuidPointerEqual(existing.OrganizationID, request.OrganizationID) || !uuidPointerEqual(existing.UnitID, request.UnitID) || (!defaultStart && !existing.StartsAt.Equal(*request.StartsAt)) || (existing.ExpiresAt == nil) != (request.ExpiresAt == nil) || (existing.ExpiresAt != nil && !existing.ExpiresAt.Equal(*request.ExpiresAt)) {
			return RoleBinding{}, ErrConflict
		}
		return existing, tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `UPDATE role_bindings SET revoked_at=expires_at WHERE membership_id=$1 AND role=$2 AND unit_id IS NOT DISTINCT FROM $3 AND revoked_at IS NULL AND expires_at<= $4`, membershipID, request.RoleCode, request.UnitID, now); err != nil {
		return RoleBinding{}, err
	}
	bindingID := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `
INSERT INTO role_bindings(id,membership_id,role,unit_id,created_by,created_at,starts_at,expires_at,grant_source,idempotency_key)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,'ACCOUNT',$9)`, bindingID, membershipID, request.RoleCode, request.UnitID, actorID, now, request.StartsAt, request.ExpiresAt, request.IdempotencyKey); err != nil {
		return RoleBinding{}, mapConstraintError(err)
	}
	version, err := bumpRoleBindingCacheVersions(ctx, tx, membershipID, request.UserID, now)
	if err != nil {
		return RoleBinding{}, err
	}
	if err := appendAudit(ctx, tx, actorID, principal.SessionID, request.OrganizationID, "role.binding.grant", "role_binding", bindingID.String(), "SUCCEEDED", map[string]any{"target_user_id": request.UserID, "role_code": request.RoleCode, "unit_id": request.UnitID, "starts_at": request.StartsAt, "expires_at": request.ExpiresAt, "grant_source": "ACCOUNT"}); err != nil {
		return RoleBinding{}, err
	}
	if err := appendOutbox(ctx, tx, "account.role-binding.changed.v1", bindingID, version, map[string]any{"organization_id": request.OrganizationID, "user_id": request.UserID, "binding_id": bindingID, "status": "GRANTED", "authorization_cache_invalidated": true}); err != nil {
		return RoleBinding{}, err
	}
	item, err := roleBindingByID(ctx, tx, bindingID)
	if err != nil {
		return RoleBinding{}, err
	}
	return item, tx.Commit(ctx)
}

func (r *PostgresRepository) RevokeOrganizationRoleBinding(ctx context.Context, principal identity.Principal, organizationID, bindingID uuid.UUID) (RoleBinding, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return RoleBinding{}, err
	}
	defer tx.Rollback(ctx)
	actorID, err := activeUserID(ctx, tx, principal)
	if err != nil {
		return RoleBinding{}, err
	}
	item, membershipID, assignable, err := lockedOrganizationRoleBinding(ctx, tx, organizationID, bindingID)
	if err != nil {
		return RoleBinding{}, err
	}
	if !assignable || item.RoleCode == "OWNER" || item.Scope == "PLATFORM" {
		return RoleBinding{}, ErrConflict
	}
	if err := requireOrganizationPermission(ctx, tx, actorID, organizationID, item.UnitID, PermissionOrganizationRolesAssign); err != nil {
		return RoleBinding{}, err
	}
	if item.RevokedAt != nil {
		return item, tx.Commit(ctx)
	}
	now := r.now()
	if _, err := tx.Exec(ctx, `UPDATE role_bindings SET revoked_at=$2,revoked_by=$3 WHERE id=$1 AND revoked_at IS NULL`, bindingID, now, actorID); err != nil {
		return RoleBinding{}, err
	}
	version, err := bumpRoleBindingCacheVersions(ctx, tx, membershipID, item.UserID, now)
	if err != nil {
		return RoleBinding{}, err
	}
	if err := appendAudit(ctx, tx, actorID, principal.SessionID, &organizationID, "role.binding.revoke", "role_binding", bindingID.String(), "SUCCEEDED", map[string]any{"target_user_id": item.UserID, "role_code": item.RoleCode, "authorization_cache_invalidated": true}); err != nil {
		return RoleBinding{}, err
	}
	if err := appendOutbox(ctx, tx, "account.role-binding.changed.v1", bindingID, version, map[string]any{"organization_id": organizationID, "user_id": item.UserID, "binding_id": bindingID, "status": "REVOKED", "authorization_cache_invalidated": true}); err != nil {
		return RoleBinding{}, err
	}
	item, err = roleBindingByID(ctx, tx, bindingID)
	if err != nil {
		return RoleBinding{}, err
	}
	return item, tx.Commit(ctx)
}

func lockedOrganizationRoleBinding(ctx context.Context, tx pgx.Tx, organizationID, bindingID uuid.UUID) (RoleBinding, uuid.UUID, bool, error) {
	var item RoleBinding
	var membershipID uuid.UUID
	var assignable bool
	err := tx.QueryRow(ctx, `
SELECT rb.id,m.user_id,rb.role,ro.scope,m.organization_id,rb.unit_id,rb.starts_at,rb.expires_at,
       CASE WHEN rb.revoked_at IS NOT NULL AND rb.expires_at IS NOT NULL AND rb.revoked_at=rb.expires_at THEN 'EXPIRED' WHEN rb.revoked_at IS NOT NULL THEN 'REVOKED' WHEN rb.expires_at IS NOT NULL AND rb.expires_at<=now() THEN 'EXPIRED' WHEN rb.starts_at>now() THEN 'SCHEDULED' ELSE 'ACTIVE' END,
       rb.grant_source,rb.created_by,rb.created_at,rb.revoked_at,rb.revoked_by,m.id,ro.assignable
FROM role_bindings rb JOIN memberships m ON m.id=rb.membership_id JOIN roles ro ON ro.code=rb.role
WHERE rb.id=$1 AND m.organization_id=$2 FOR UPDATE OF rb,m`, bindingID, organizationID).Scan(
		&item.ID, &item.UserID, &item.RoleCode, &item.Scope, &item.OrganizationID, &item.UnitID, &item.StartsAt, &item.ExpiresAt, &item.Status, &item.GrantSource, &item.CreatedBy, &item.CreatedAt, &item.RevokedAt, &item.RevokedBy, &membershipID, &assignable,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return RoleBinding{}, uuid.Nil, false, ErrNotFound
	}
	return item, membershipID, assignable, err
}

func roleBindingByIdempotencyKey(ctx context.Context, tx pgx.Tx, membershipID uuid.UUID, key string) (RoleBinding, bool, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM role_bindings WHERE membership_id=$1 AND idempotency_key=$2`, membershipID, key).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return RoleBinding{}, false, nil
	}
	if err != nil {
		return RoleBinding{}, false, err
	}
	item, err := roleBindingByID(ctx, tx, id)
	return item, true, err
}

func roleBindingByID(ctx context.Context, database roleBindingQuerier, bindingID uuid.UUID) (RoleBinding, error) {
	items, err := queryRoleBindings(ctx, database, `ra.id=$1`, bindingID)
	if err != nil {
		return RoleBinding{}, err
	}
	if len(items) == 0 {
		return RoleBinding{}, ErrNotFound
	}
	return items[0], nil
}

func bumpRoleBindingCacheVersions(ctx context.Context, tx pgx.Tx, membershipID, userID uuid.UUID, now time.Time) (int64, error) {
	var version int64
	var organizationID uuid.UUID
	var status string
	if err := tx.QueryRow(ctx, `UPDATE memberships SET version=version+1,updated_at=$2 WHERE id=$1 RETURNING organization_id,status,version`, membershipID, now).Scan(&organizationID, &status, &version); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET membership_version=membership_version+1,updated_at=$2 WHERE id=$1`, userID, now); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE user_sessions SET membership_version=membership_version+1,last_seen_at=$2 WHERE user_id=$1`, userID, now); err != nil {
		return 0, err
	}
	if err := appendMembershipChanged(ctx, tx, membershipID, organizationID, userID, status, version); err != nil {
		return 0, err
	}
	return version, nil
}

type RoleBindingExpiryWorker struct {
	repository *PostgresRepository
	interval   time.Duration
	limit      int
}

var roleBindingExpiryPrincipal = identity.Principal{Issuer: "pinvou-system", Subject: "role-binding-expiry-worker", SessionID: "worker"}

func NewRoleBindingExpiryWorker(repository *PostgresRepository, interval time.Duration) *RoleBindingExpiryWorker {
	if interval <= 0 {
		interval = time.Minute
	}
	return &RoleBindingExpiryWorker{repository: repository, interval: interval, limit: 100}
}

func (w *RoleBindingExpiryWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		_, _ = w.repository.ExpireRoleBindings(ctx, w.limit)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *PostgresRepository) ExpireRoleBindings(ctx context.Context, limit int) (int, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx, `SELECT id FROM role_bindings WHERE revoked_at IS NULL AND expires_at<= $1 ORDER BY expires_at,id LIMIT $2`, r.now(), limit)
	if err != nil {
		return 0, err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	completed := 0
	for _, id := range ids {
		changed, err := r.expireRoleBinding(ctx, id)
		if err != nil {
			return completed, err
		}
		if changed {
			completed++
		}
	}
	remaining := limit - completed
	if remaining <= 0 {
		return completed, nil
	}
	assignmentRows, err := r.pool.Query(ctx, `SELECT id FROM role_assignments WHERE source<>'LEGACY_ROLE_BINDING' AND revoked_at IS NULL AND expires_at<=$1 ORDER BY expires_at,id LIMIT $2`, r.now(), remaining)
	if err != nil {
		return completed, err
	}
	ids = ids[:0]
	for assignmentRows.Next() {
		var id uuid.UUID
		if err := assignmentRows.Scan(&id); err != nil {
			assignmentRows.Close()
			return completed, err
		}
		ids = append(ids, id)
	}
	if err := assignmentRows.Err(); err != nil {
		assignmentRows.Close()
		return completed, err
	}
	assignmentRows.Close()
	for _, id := range ids {
		changed, err := r.expireDirectRoleAssignment(ctx, id)
		if err != nil {
			return completed, err
		}
		if changed {
			completed++
		}
	}
	return completed, nil
}

func (r *PostgresRepository) expireRoleBinding(ctx context.Context, bindingID uuid.UUID) (bool, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var membershipID, userID, organizationID uuid.UUID
	var expiresAt time.Time
	err = tx.QueryRow(ctx, `SELECT rb.membership_id,m.user_id,m.organization_id,rb.expires_at FROM role_bindings rb JOIN memberships m ON m.id=rb.membership_id WHERE rb.id=$1 AND rb.revoked_at IS NULL AND rb.expires_at<= $2 FOR UPDATE OF rb,m`, bindingID, r.now()).Scan(&membershipID, &userID, &organizationID, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE role_bindings SET revoked_at=expires_at WHERE id=$1 AND revoked_at IS NULL`, bindingID); err != nil {
		return false, err
	}
	version, err := bumpRoleBindingCacheVersions(ctx, tx, membershipID, userID, r.now())
	if err != nil {
		return false, err
	}
	if err := appendAdminAudit(ctx, tx, nil, roleBindingExpiryPrincipal, &organizationID, "role.binding.expire", "role_binding", bindingID.String(), map[string]any{"user_id": userID, "expires_at": expiresAt, "authorization_cache_invalidated": true}); err != nil {
		return false, err
	}
	if err := appendOutbox(ctx, tx, "account.role-binding.changed.v1", bindingID, version, map[string]any{"organization_id": organizationID, "user_id": userID, "binding_id": bindingID, "status": "EXPIRED", "authorization_cache_invalidated": true}); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (r *PostgresRepository) expireDirectRoleAssignment(ctx context.Context, bindingID uuid.UUID) (bool, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var userID uuid.UUID
	var organizationID *uuid.UUID
	var expiresAt time.Time
	err = tx.QueryRow(ctx, `SELECT user_id,organization_id,expires_at FROM role_assignments WHERE id=$1 AND source<>'LEGACY_ROLE_BINDING' AND revoked_at IS NULL AND expires_at<=$2 FOR UPDATE`, bindingID, r.now()).Scan(&userID, &organizationID, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE role_assignments SET revoked_at=expires_at WHERE id=$1 AND revoked_at IS NULL`, bindingID); err != nil {
		return false, err
	}
	var version int64
	if organizationID == nil {
		version, err = bumpPlatformRoleCacheVersions(ctx, tx, userID, r.now())
	} else {
		var membershipID uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT id FROM memberships WHERE organization_id=$1 AND user_id=$2`, *organizationID, userID).Scan(&membershipID); err == nil {
			version, err = bumpRoleBindingCacheVersions(ctx, tx, membershipID, userID, r.now())
		}
	}
	if err != nil {
		return false, err
	}
	if err := appendAdminAudit(ctx, tx, nil, roleBindingExpiryPrincipal, organizationID, "role.binding.expire", "role_binding", bindingID.String(), map[string]any{"user_id": userID, "expires_at": expiresAt, "authorization_cache_invalidated": true}); err != nil {
		return false, err
	}
	if err := appendOutbox(ctx, tx, "account.role-binding.changed.v1", bindingID, version, map[string]any{"organization_id": organizationID, "user_id": userID, "binding_id": bindingID, "status": "EXPIRED", "authorization_cache_invalidated": true}); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func appendMembershipChanged(ctx context.Context, tx pgx.Tx, membershipID, organizationID, userID uuid.UUID, status string, version int64) error {
	return appendOutbox(ctx, tx, "account.membership.changed.v1", membershipID, version, map[string]any{
		"organization_id": organizationID, "user_id": userID,
		"membership_status": status, "membership_version": version,
	})
}
