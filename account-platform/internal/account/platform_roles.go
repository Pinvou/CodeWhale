package account

import (
	"context"
	"errors"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	PermissionPlatformRolesRead   = "platform.roles.read"
	PermissionPlatformRolesAssign = "platform.roles.assign"
)

func (r *PostgresRepository) PlatformRoleDefinitions(ctx context.Context) ([]AdminRoleDefinition, error) {
	rows, err := r.pool.Query(ctx, `SELECT ro.code,ro.scope,ro.assignable,ro.description,COALESCE(array_agg(rp.permission_code ORDER BY rp.permission_code) FILTER (WHERE rp.permission_code IS NOT NULL),'{}'::text[]) FROM roles ro LEFT JOIN role_permissions rp ON rp.role_code=ro.code WHERE ro.scope='PLATFORM' GROUP BY ro.code ORDER BY ro.code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AdminRoleDefinition, 0)
	for rows.Next() {
		var item AdminRoleDefinition
		if err = rows.Scan(&item.Code, &item.Scope, &item.Assignable, &item.Description, &item.Permissions); err != nil {
			return nil, err
		}
		item.Permissions = filterF01Permissions(item.Permissions)
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminPlatformRoleBindings(ctx context.Context, userID *uuid.UUID) (RoleBindingList, error) {
	condition := `ro.scope='PLATFORM' AND ra.organization_id IS NULL AND ra.unit_id IS NULL`
	var args []any
	if userID != nil {
		condition += ` AND ra.user_id=$1`
		args = append(args, *userID)
	}
	items, truncated, err := r.queryRoleBindingsBounded(ctx, condition, args...)
	return RoleBindingList{Items: items, Truncated: truncated}, err
}

// New platform HTTP writers revalidate live human authority in the same
// transaction as the role mutation. Users are locked in UUID order before any
// assignments, including when actor=target or administrators manage each other.
// Generic organization/internal repository callers retain their existing entry.
func lockPlatformRoleWriter(ctx context.Context, tx pgx.Tx, principal identity.Principal, target uuid.UUID, requireActiveTarget bool) (uuid.UUID, error) {
	var actor uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT user_id FROM identities WHERE issuer=$1 AND subject=$2`, principal.Issuer, principal.Subject).Scan(&actor); errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrForbidden
	} else if err != nil {
		return uuid.Nil, err
	}
	rows, err := tx.Query(ctx, `SELECT id,status FROM users WHERE id=ANY($1::uuid[]) ORDER BY id FOR UPDATE`, []uuid.UUID{actor, target})
	if err != nil {
		return uuid.Nil, err
	}
	active := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		var status string
		if err = rows.Scan(&id, &status); err != nil {
			rows.Close()
			return uuid.Nil, err
		}
		active[id] = status == "ACTIVE"
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return uuid.Nil, err
	}
	if !active[actor] {
		return uuid.Nil, ErrForbidden
	}
	if _, exists := active[target]; !exists {
		return uuid.Nil, ErrNotFound
	}
	if requireActiveTarget && !active[target] {
		return uuid.Nil, ErrConflict
	}
	rows, err = tx.Query(ctx, `SELECT ra.id FROM role_assignments ra JOIN roles ro ON ro.code=ra.role_code AND ro.scope='PLATFORM' JOIN role_permissions rp ON rp.role_code=ra.role_code AND rp.permission_code=$2 WHERE ra.user_id=$1 AND ra.organization_id IS NULL AND ra.unit_id IS NULL AND ra.revoked_at IS NULL ORDER BY ra.id FOR SHARE OF ra`, actor, PermissionPlatformRolesAssign)
	if err != nil {
		return uuid.Nil, err
	}
	for rows.Next() {
		var id uuid.UUID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return uuid.Nil, err
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return uuid.Nil, err
	}
	// clock_timestamp is evaluated after both users and authorization rows are
	// locked, so waiting for a lock cannot preserve expired authority.
	decision, err := authorizePermission(ctx, tx, principal, PermissionPlatformRolesAssign, nil, nil, true)
	if err != nil {
		return uuid.Nil, err
	}
	if !decision.Allowed {
		return uuid.Nil, ErrForbidden
	}
	return actor, nil
}

func validatePlatformRoleGrant(request RoleBindingGrant, principal identity.Principal, now time.Time) error {
	if request.StartsAt == nil {
		return ErrConflict
	}
	if request.ExpiresAt != nil && (!request.ExpiresAt.After(*request.StartsAt) || !request.ExpiresAt.After(now)) {
		return ErrConflict
	}
	if request.RoleCode == "PLATFORM_AUDIT_PII_EXPORT" {
		if request.ExpiresAt == nil || request.ExpiresAt.Sub(*request.StartsAt) > 24*time.Hour || len(request.Reason) < 10 || len(request.Reason) > 500 {
			return ErrConflict
		}
		if !recentlyAuthenticated(principal.AuthenticatedAt, now) {
			return ErrRecentAuthenticationRequired
		}
	}
	return nil
}

// The global admin key lookup prevents changing the subject or organizational
// namespace to bypass complete-intent comparison. Old duplicate keys are
// ambiguous and therefore rejected without new authorization effects.
func adminRoleGrantByKey(ctx context.Context, tx pgx.Tx, key string) (RoleBinding, bool, error) {
	rows, err := tx.Query(ctx, `SELECT id FROM role_assignments WHERE idempotency_key=$1 AND grant_source='ADMIN' ORDER BY id LIMIT 2`, key)
	if err != nil {
		return RoleBinding{}, false, err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return RoleBinding{}, false, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return RoleBinding{}, false, err
	}
	if len(ids) > 1 {
		return RoleBinding{}, false, ErrConflict
	}
	if len(ids) == 0 {
		return RoleBinding{}, false, nil
	}
	item, err := roleBindingByID(ctx, tx, ids[0])
	return item, true, err
}

// A replay recovers the persisted current projection, never a new grant. The
// existing audit is committed atomically with the first grant and retains its
// issuer/subject and reason. Missing or ambiguous history cannot prove equality.
func matchAdminRoleGrantReplay(ctx context.Context, tx pgx.Tx, item RoleBinding, principal identity.Principal, request RoleBindingGrant, actorID *uuid.UUID, explicitStartsAt bool) error {
	sameActor := item.CreatedBy == nil && actorID == nil || item.CreatedBy != nil && actorID != nil && *item.CreatedBy == *actorID
	if !sameActor || item.UserID != request.UserID || item.RoleCode != request.RoleCode || !uuidPointerEqual(item.OrganizationID, request.OrganizationID) || !uuidPointerEqual(item.UnitID, request.UnitID) || item.GrantSource != "ADMIN" {
		return ErrConflict
	}
	if explicitStartsAt && !item.StartsAt.Equal(*request.StartsAt) {
		return ErrConflict
	}
	if (item.ExpiresAt == nil) != (request.ExpiresAt == nil) || item.ExpiresAt != nil && !item.ExpiresAt.Equal(*request.ExpiresAt) {
		return ErrConflict
	}
	var reason, issuer, subject string
	err := tx.QueryRow(ctx, `SELECT COALESCE(change_summary->>'reason',''),COALESCE(change_summary->>'actor_issuer',''),COALESCE(change_summary->>'actor_subject','') FROM audit_logs WHERE resource_type='role_binding' AND resource_id=$1 AND action='admin.role.binding.grant' AND result='SUCCEEDED' ORDER BY occurred_at,id LIMIT 1`, item.ID.String()).Scan(&reason, &issuer, &subject)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if reason != request.Reason || issuer != principal.Issuer || subject != principal.Subject {
		return ErrConflict
	}
	return nil
}
