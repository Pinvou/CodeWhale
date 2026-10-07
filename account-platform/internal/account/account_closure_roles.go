package account

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func assertNotLastActiveSuperAdmin(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error {
	var targetIsSuperAdmin, anotherActiveSuperAdmin bool
	err := tx.QueryRow(ctx, `
SELECT
  EXISTS(SELECT 1 FROM users u JOIN role_assignments ra ON ra.user_id=u.id
    WHERE u.id=$1 AND u.status='ACTIVE' AND ra.role_code='PLATFORM_SUPER_ADMIN'
      AND ra.organization_id IS NULL AND ra.revoked_at IS NULL AND ra.starts_at<=now()
      AND (ra.expires_at IS NULL OR ra.expires_at>now())),
  EXISTS(SELECT 1 FROM users u JOIN role_assignments ra ON ra.user_id=u.id
    WHERE u.id<>$1 AND u.status='ACTIVE' AND ra.role_code='PLATFORM_SUPER_ADMIN'
      AND ra.organization_id IS NULL AND ra.revoked_at IS NULL AND ra.starts_at<=now()
      AND (ra.expires_at IS NULL OR ra.expires_at>now()))`, userID).Scan(&targetIsSuperAdmin, &anotherActiveSuperAdmin)
	if err != nil {
		return err
	}
	if targetIsSuperAdmin && !anotherActiveSuperAdmin {
		return ErrLastActiveSuperAdmin
	}
	return nil
}
