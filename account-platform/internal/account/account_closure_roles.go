package account

import (
	"context"
	"errors"
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

// RevokeAccountClosureRoleBindings atomically invalidates every effective and
// scheduled binding after the closure gate passes. It remains the reusable
// primitive for callers outside the full cleanup preparation transaction.
func (r *PostgresRepository) RevokeAccountClosureRoleBindings(ctx context.Context, userID uuid.UUID) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&status); errors.Is(err, pgx.ErrNoRows) || status == "DELETED" {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if err := assertNotLastActiveSuperAdmin(ctx, tx, userID); err != nil {
		return err
	}
	var ownsOrganization bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM memberships m JOIN role_bindings rb ON rb.membership_id=m.id AND rb.role='OWNER' WHERE m.user_id=$1 AND m.status IN ('ACTIVE','SUSPENDED') AND rb.revoked_at IS NULL AND rb.starts_at<=now() AND (rb.expires_at IS NULL OR rb.expires_at>now()))`, userID).Scan(&ownsOrganization); err != nil {
		return err
	}
	if ownsOrganization {
		return ErrLastOwner
	}
	now := r.now()
	if _, err := tx.Exec(ctx, `UPDATE role_bindings rb SET revoked_at=$2,revoked_by=$1 FROM memberships m WHERE rb.membership_id=m.id AND m.user_id=$1 AND rb.revoked_at IS NULL`, userID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE role_assignments SET revoked_at=$2,revoked_by=$1 WHERE user_id=$1 AND source<>'LEGACY_ROLE_BINDING' AND revoked_at IS NULL`, userID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET membership_version=membership_version+1,version=version+1,updated_at=$2 WHERE id=$1`, userID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_sessions WHERE user_id=$1`, userID); err != nil {
		return err
	}
	if err := appendSecurityClosureAudit(ctx, tx, userID, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func appendSecurityClosureAudit(ctx context.Context, tx pgx.Tx, userID uuid.UUID, now time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit_logs(id,actor_user_id,action,resource_type,resource_id,result,request_id,trace_id,change_summary,occurred_at) VALUES($1,$2,'security.account-closure.roles-revoked','user',$3,'SUCCEEDED',$4,$4,'{"sessions_revoked":true,"all_bindings_revoked":true}'::jsonb,$5)`, uuid.Must(uuid.NewV7()), userID, userID.String(), "account-closure-"+uuid.NewString(), now)
	return err
}
