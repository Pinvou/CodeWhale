package account

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type platformAuthorizationRequirement struct {
	UserID     uuid.UUID
	Permission string
}

type lockedPlatformAuthorization struct {
	platformAuthorizationRequirement
	Bindings []uuid.UUID
	Version  int64
}

// Acquire every actor before any binding, and every binding before any
// authorization version. Multi-actor execution admission uses the same order
// as a single-actor write, rather than interleaving two complete lock chains.
// Time predicates belong to the read after all of these locks, not to this
// candidate lock set: scheduled expiry does not update a row or raise 40001.
func lockPlatformAuthorizationFacts(ctx context.Context, database queryable, requirements ...platformAuthorizationRequirement) ([]lockedPlatformAuthorization, error) {
	order := make([]int, len(requirements))
	locked := make([]lockedPlatformAuthorization, len(requirements))
	for i, requirement := range requirements {
		order[i] = i
		locked[i].platformAuthorizationRequirement = requirement
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := requirements[order[i]], requirements[order[j]]
		if a.UserID != b.UserID {
			return a.UserID.String() < b.UserID.String()
		}
		return a.Permission < b.Permission
	})
	for _, i := range order {
		var actor uuid.UUID
		err := database.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR SHARE`, requirements[i].UserID).Scan(&actor)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrForbidden
		}
		if err != nil {
			return nil, err
		}
	}
	for _, i := range order {
		err := database.QueryRow(ctx, `SELECT COALESCE(array_agg(locked.id ORDER BY locked.id),'{}'::uuid[]) FROM (
SELECT ra.id FROM role_assignments ra
JOIN role_permissions rp ON rp.role_code=ra.role_code AND rp.permission_code=$2
WHERE ra.user_id=$1 AND ra.organization_id IS NULL AND ra.revoked_at IS NULL
ORDER BY ra.id FOR SHARE OF ra
) locked`, requirements[i].UserID, requirements[i].Permission).Scan(&locked[i].Bindings)
		if err != nil {
			return nil, err
		}
	}
	for _, i := range order {
		err := database.QueryRow(ctx, `SELECT version FROM authorization_versions WHERE user_id=$1 AND scope='PLATFORM' AND scope_id='00000000-0000-0000-0000-000000000000'::uuid FOR SHARE`, requirements[i].UserID).Scan(&locked[i].Version)
		if errors.Is(err, pgx.ErrNoRows) {
			locked[i].Version = 1
		} else if err != nil {
			return nil, err
		}
	}
	return locked, nil
}

func approvalDatabaseTime(ctx context.Context, database queryable) (time.Time, error) {
	var now time.Time
	err := database.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now)
	return now, err
}

func finalPlatformAuthorization(ctx context.Context, database queryable, userID uuid.UUID, permission string) (uuid.UUID, int64, *time.Time, time.Time, error) {
	locked, err := lockPlatformAuthorizationFacts(ctx, database, platformAuthorizationRequirement{UserID: userID, Permission: permission})
	if err != nil {
		return uuid.Nil, 0, nil, time.Time{}, err
	}
	now, err := approvalDatabaseTime(ctx, database)
	if err != nil {
		return uuid.Nil, 0, nil, time.Time{}, err
	}
	binding, version, until, err := platformAuthorizationAt(ctx, database, locked[0], now)
	return binding, version, until, now, err
}

// The bindings and actor belong to the caller's already held lock set. Never
// switch to an unlocked binding that appeared after the acquisition statements.
func platformAuthorizationAt(ctx context.Context, database queryable, locked lockedPlatformAuthorization, now time.Time) (uuid.UUID, int64, *time.Time, error) {
	var binding uuid.UUID
	var until *time.Time
	err := database.QueryRow(ctx, `SELECT ra.id,ra.expires_at FROM users u
JOIN role_assignments ra ON ra.user_id=u.id
JOIN role_permissions rp ON rp.role_code=ra.role_code AND rp.permission_code=$2
WHERE u.id=$1 AND u.status='ACTIVE' AND ra.id=ANY($3::uuid[]) AND ra.organization_id IS NULL
AND ra.revoked_at IS NULL AND ra.starts_at<=$4 AND (ra.expires_at IS NULL OR ra.expires_at>$4)
ORDER BY ra.expires_at NULLS LAST,ra.id LIMIT 1`, locked.UserID, locked.Permission, locked.Bindings, now).Scan(&binding, &until)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, 0, nil, ErrForbidden
	}
	return binding, locked.Version, until, err
}
