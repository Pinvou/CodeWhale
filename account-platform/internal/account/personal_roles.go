package account

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const RolePersonalUser = "PERSONAL_USER"

func personalRoleEligibleStatus(status string) bool {
	switch status {
	case "BOOTSTRAP_PENDING", "PROVISIONING", "ACTIVE", "REPAIR_REQUIRED":
		return true
	default:
		return false
	}
}

func ensureDefaultPersonalRole(ctx context.Context, tx pgx.Tx, userID uuid.UUID, now time.Time) error {
	_, err := tx.Exec(ctx, `
INSERT INTO role_assignments(
    id,user_id,role_code,organization_id,unit_id,created_by,source,grant_source,
    starts_at,expires_at,revoked_at,revoked_by,idempotency_key,created_at
) VALUES($1,$2,$3,NULL,NULL,NULL,'ACCOUNT_LIFECYCLE','ACCOUNT_LIFECYCLE',$4,NULL,NULL,NULL,'personal-user-default',$4)
ON CONFLICT DO NOTHING`, uuid.Must(uuid.NewV7()), userID, RolePersonalUser, now)
	return err
}
