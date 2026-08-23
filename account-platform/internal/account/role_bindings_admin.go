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

func (r *PostgresRepository) AdminRoleBindings(ctx context.Context, userID, organizationID *uuid.UUID) ([]RoleBinding, error) {
	switch {
	case userID != nil && organizationID != nil:
		return r.queryRoleBindings(ctx, `ra.user_id=$1 AND ra.organization_id=$2`, *userID, *organizationID)
	case userID != nil:
		return r.queryRoleBindings(ctx, `ra.user_id=$1`, *userID)
	case organizationID != nil:
		return r.queryRoleBindings(ctx, `ra.organization_id=$1`, *organizationID)
	default:
		return r.queryRoleBindings(ctx, `TRUE`)
	}
}

func (r *PostgresRepository) AdminGrantRoleBinding(ctx context.Context, principal identity.Principal, request RoleBindingGrant) (RoleBinding, error) {
	now := r.now()
	request.RoleCode = strings.ToUpper(strings.TrimSpace(request.RoleCode))
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	if request.UserID == uuid.Nil || request.RoleCode == "" || len(request.IdempotencyKey) < 8 || len(request.IdempotencyKey) > 120 {
		return RoleBinding{}, ErrConflict
	}
	if request.StartsAt == nil {
		request.StartsAt = &now
	} else {
		value := request.StartsAt.UTC()
		request.StartsAt = &value
	}
	if request.ExpiresAt != nil {
		value := request.ExpiresAt.UTC()
		request.ExpiresAt = &value
		if !value.After(*request.StartsAt) || !value.After(now) {
			return RoleBinding{}, ErrConflict
		}
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return RoleBinding{}, err
	}
	defer tx.Rollback(ctx)
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return RoleBinding{}, err
	}
	var userStatus, scope string
	var assignable bool
	if err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1 FOR UPDATE`, request.UserID).Scan(&userStatus); errors.Is(err, pgx.ErrNoRows) {
		return RoleBinding{}, ErrNotFound
	} else if err != nil {
		return RoleBinding{}, err
	}
	if userStatus != "ACTIVE" {
		return RoleBinding{}, ErrConflict
	}
	if err := tx.QueryRow(ctx, `SELECT scope,assignable FROM roles WHERE code=$1`, request.RoleCode).Scan(&scope, &assignable); errors.Is(err, pgx.ErrNoRows) {
		return RoleBinding{}, ErrConflict
	} else if err != nil {
		return RoleBinding{}, err
	}
	// Super administrator lifecycle and last-administrator protection belong to APT-007.
	if !assignable || request.RoleCode == "PLATFORM_SUPER_ADMIN" || request.RoleCode == "OWNER" {
		return RoleBinding{}, ErrConflict
	}
	if scope == "PLATFORM" {
		if request.OrganizationID != nil || request.UnitID != nil {
			return RoleBinding{}, ErrConflict
		}
		var existingID uuid.UUID
		err := tx.QueryRow(ctx, `SELECT id FROM role_assignments WHERE user_id=$1 AND organization_id IS NULL AND idempotency_key=$2`, request.UserID, request.IdempotencyKey).Scan(&existingID)
		if err == nil {
			item, err := roleBindingByID(ctx, tx, existingID)
			if err != nil {
				return RoleBinding{}, err
			}
			return item, tx.Commit(ctx)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return RoleBinding{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE role_assignments SET revoked_at=expires_at WHERE user_id=$1 AND role_code=$2 AND organization_id IS NULL AND revoked_at IS NULL AND expires_at<=$3`, request.UserID, request.RoleCode, now); err != nil {
			return RoleBinding{}, err
		}
		bindingID := uuid.Must(uuid.NewV7())
		if _, err := tx.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,created_by,source,grant_source,starts_at,expires_at,idempotency_key,created_at) VALUES($1,$2,$3,$4,'ADMIN','ADMIN',$5,$6,$7,$8)`, bindingID, request.UserID, request.RoleCode, actorID, request.StartsAt, request.ExpiresAt, request.IdempotencyKey, now); err != nil {
			return RoleBinding{}, mapConstraintError(err)
		}
		version, err := bumpPlatformRoleCacheVersions(ctx, tx, request.UserID, now)
		if err != nil {
			return RoleBinding{}, err
		}
		if err := appendAdminAudit(ctx, tx, actorID, principal, nil, "admin.role.binding.grant", "role_binding", bindingID.String(), map[string]any{"user_id": request.UserID, "role_code": request.RoleCode, "scope": scope, "starts_at": request.StartsAt, "expires_at": request.ExpiresAt}); err != nil {
			return RoleBinding{}, err
		}
		if err := appendOutbox(ctx, tx, "account.role-binding.changed.v1", bindingID, version, map[string]any{"user_id": request.UserID, "binding_id": bindingID, "status": "GRANTED", "scope": "PLATFORM", "authorization_cache_invalidated": true}); err != nil {
			return RoleBinding{}, err
		}
		item, err := roleBindingByID(ctx, tx, bindingID)
		if err != nil {
			return RoleBinding{}, err
		}
		return item, tx.Commit(ctx)
	}
	if request.OrganizationID == nil || *request.OrganizationID == uuid.Nil || (scope == "UNIT") != (request.UnitID != nil) {
		return RoleBinding{}, ErrConflict
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
		return existing, tx.Commit(ctx)
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
	if _, err := tx.Exec(ctx, `UPDATE role_bindings SET revoked_at=expires_at WHERE membership_id=$1 AND role=$2 AND unit_id IS NOT DISTINCT FROM $3 AND revoked_at IS NULL AND expires_at<=$4`, membershipID, request.RoleCode, request.UnitID, now); err != nil {
		return RoleBinding{}, err
	}
	createdBy := request.UserID
	if actorID != nil {
		createdBy = *actorID
	}
	bindingID := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO role_bindings(id,membership_id,role,unit_id,created_by,created_at,starts_at,expires_at,grant_source,idempotency_key) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'ADMIN',$9)`, bindingID, membershipID, request.RoleCode, request.UnitID, createdBy, now, request.StartsAt, request.ExpiresAt, request.IdempotencyKey); err != nil {
		return RoleBinding{}, mapConstraintError(err)
	}
	version, err := bumpRoleBindingCacheVersions(ctx, tx, membershipID, request.UserID, now)
	if err != nil {
		return RoleBinding{}, err
	}
	if err := appendAdminAudit(ctx, tx, actorID, principal, request.OrganizationID, "admin.role.binding.grant", "role_binding", bindingID.String(), map[string]any{"user_id": request.UserID, "role_code": request.RoleCode, "unit_id": request.UnitID, "starts_at": request.StartsAt, "expires_at": request.ExpiresAt}); err != nil {
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

func (r *PostgresRepository) AdminRevokeRoleBinding(ctx context.Context, principal identity.Principal, bindingID uuid.UUID) (RoleBinding, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return RoleBinding{}, err
	}
	defer tx.Rollback(ctx)
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return RoleBinding{}, err
	}
	var item RoleBinding
	var source string
	var assignable bool
	err = tx.QueryRow(ctx, `
SELECT ra.id,ra.user_id,ra.role_code,ro.scope,ra.organization_id,ra.unit_id,ra.starts_at,ra.expires_at,
       CASE WHEN ra.revoked_at IS NOT NULL AND ra.expires_at IS NOT NULL AND ra.revoked_at=ra.expires_at THEN 'EXPIRED' WHEN ra.revoked_at IS NOT NULL THEN 'REVOKED' WHEN ra.expires_at IS NOT NULL AND ra.expires_at<=now() THEN 'EXPIRED' WHEN ra.starts_at>now() THEN 'SCHEDULED' ELSE 'ACTIVE' END,
       ra.grant_source,ra.created_by,ra.created_at,ra.revoked_at,ra.revoked_by,ra.source,ro.assignable
FROM role_assignments ra JOIN roles ro ON ro.code=ra.role_code WHERE ra.id=$1 FOR UPDATE OF ra`, bindingID).Scan(
		&item.ID, &item.UserID, &item.RoleCode, &item.Scope, &item.OrganizationID, &item.UnitID, &item.StartsAt, &item.ExpiresAt, &item.Status, &item.GrantSource, &item.CreatedBy, &item.CreatedAt, &item.RevokedAt, &item.RevokedBy, &source, &assignable,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return RoleBinding{}, ErrNotFound
	}
	if err != nil {
		return RoleBinding{}, err
	}
	if !assignable || item.RoleCode == "PLATFORM_SUPER_ADMIN" || item.RoleCode == "OWNER" {
		return RoleBinding{}, ErrConflict
	}
	if item.RevokedAt != nil {
		return item, tx.Commit(ctx)
	}
	now := r.now()
	if source == "LEGACY_ROLE_BINDING" {
		if _, err := tx.Exec(ctx, `UPDATE role_bindings SET revoked_at=$2,revoked_by=$3 WHERE id=$1 AND revoked_at IS NULL`, bindingID, now, actorID); err != nil {
			return RoleBinding{}, err
		}
	} else if _, err := tx.Exec(ctx, `UPDATE role_assignments SET revoked_at=$2,revoked_by=$3 WHERE id=$1 AND revoked_at IS NULL`, bindingID, now, actorID); err != nil {
		return RoleBinding{}, err
	}
	var version int64
	if item.OrganizationID != nil {
		var membershipID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM memberships WHERE organization_id=$1 AND user_id=$2`, *item.OrganizationID, item.UserID).Scan(&membershipID); err != nil {
			return RoleBinding{}, err
		}
		version, err = bumpRoleBindingCacheVersions(ctx, tx, membershipID, item.UserID, now)
	} else {
		version, err = bumpPlatformRoleCacheVersions(ctx, tx, item.UserID, now)
	}
	if err != nil {
		return RoleBinding{}, err
	}
	if err := appendAdminAudit(ctx, tx, actorID, principal, item.OrganizationID, "admin.role.binding.revoke", "role_binding", bindingID.String(), map[string]any{"user_id": item.UserID, "role_code": item.RoleCode, "authorization_cache_invalidated": true}); err != nil {
		return RoleBinding{}, err
	}
	if err := appendOutbox(ctx, tx, "account.role-binding.changed.v1", bindingID, version, map[string]any{"organization_id": item.OrganizationID, "user_id": item.UserID, "binding_id": bindingID, "status": "REVOKED", "authorization_cache_invalidated": true}); err != nil {
		return RoleBinding{}, err
	}
	item, err = roleBindingByID(ctx, tx, bindingID)
	if err != nil {
		return RoleBinding{}, err
	}
	return item, tx.Commit(ctx)
}

func bumpPlatformRoleCacheVersions(ctx context.Context, tx pgx.Tx, userID uuid.UUID, now time.Time) (int64, error) {
	// Keep context caches aligned with the database-triggered authorization version.
	if _, err := tx.Exec(ctx, `UPDATE users SET membership_version=membership_version+1,updated_at=$2 WHERE id=$1`, userID, now); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE user_sessions SET membership_version=membership_version+1,last_seen_at=$2 WHERE user_id=$1`, userID, now); err != nil {
		return 0, err
	}
	var version int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT version FROM authorization_versions WHERE user_id=$1 AND scope='PLATFORM' AND scope_id='00000000-0000-0000-0000-000000000000'::uuid),1)`, userID).Scan(&version); err != nil {
		return 0, err
	}
	return version, nil
}
