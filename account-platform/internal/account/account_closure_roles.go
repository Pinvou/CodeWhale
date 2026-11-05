package account

import (
	"context"
	"time"

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

// CheckAccountClosureRoleGate is the reusable ACC-48 precondition. Ordinary,
// scheduled, temporary and break-glass bindings never block closure; ownership
// and the final effective super-administrator identity do.
func (r *PostgresRepository) CheckAccountClosureRoleGate(ctx context.Context, userID uuid.UUID) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	return checkAccountClosureRoleGateTx(ctx, tx, userID)
}

func checkAccountClosureRoleGateTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error {
	var ownsOrganization bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(
SELECT 1 FROM memberships m JOIN role_bindings rb ON rb.membership_id=m.id AND rb.role='OWNER'
JOIN organizations o ON o.id=m.organization_id AND o.status<>'DELETED'
WHERE m.user_id=$1 AND m.status IN ('ACTIVE','SUSPENDED') AND rb.revoked_at IS NULL
  AND rb.starts_at<=now() AND (rb.expires_at IS NULL OR rb.expires_at>now()))`, userID).Scan(&ownsOrganization); err != nil {
		return err
	}
	if ownsOrganization {
		return ErrLastOwner
	}
	return assertNotLastActiveSuperAdmin(ctx, tx, userID)
}

func appendSecurityClosureAudit(ctx context.Context, tx pgx.Tx, userID uuid.UUID, now time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit_logs(id,actor_user_id,action,resource_type,resource_id,result,request_id,trace_id,change_summary,occurred_at) VALUES($1,$2,'security.account-closure.roles-revoked','user',$3,'SUCCEEDED',$4,$4,'{"sessions_revoked":true,"all_bindings_revoked":true}'::jsonb,$5)`, uuid.Must(uuid.NewV7()), userID, userID.String(), "account-closure-"+uuid.NewString(), now)
	return err
}
