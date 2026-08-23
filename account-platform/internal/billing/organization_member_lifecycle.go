package billing

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type OrganizationMemberLifecycleState struct {
	OrganizationID    uuid.UUID `json:"organization_id"`
	UserID            uuid.UUID `json:"user_id"`
	Status            string    `json:"status"`
	MembershipVersion int64     `json:"membership_version"`
}

func (r *PostgresRepository) ApplyOrganizationMemberLifecycle(ctx context.Context, organizationID, userID uuid.UUID, status string, membershipVersion int64) (OrganizationMemberLifecycleState, error) {
	status = strings.ToUpper(strings.TrimSpace(status))
	if organizationID == uuid.Nil || userID == uuid.Nil || membershipVersion < 1 || (status != "ACTIVE" && status != "REVOKED") {
		return OrganizationMemberLifecycleState{}, ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return OrganizationMemberLifecycleState{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "organization-member:"+organizationID.String()+":"+userID.String()); err != nil {
		return OrganizationMemberLifecycleState{}, err
	}
	var currentStatus string
	var currentVersion int64
	err = tx.QueryRow(ctx, `SELECT status,membership_version FROM organization_member_lifecycle_states WHERE organization_id=$1 AND user_id=$2 FOR UPDATE`, organizationID, userID).Scan(&currentStatus, &currentVersion)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return OrganizationMemberLifecycleState{}, err
	}
	if err == nil && (membershipVersion < currentVersion || (membershipVersion == currentVersion && status != currentStatus)) {
		return OrganizationMemberLifecycleState{}, ErrConflict
	}
	if _, err := tx.Exec(ctx, `INSERT INTO organization_member_lifecycle_states(organization_id,user_id,status,membership_version,updated_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT(organization_id,user_id) DO UPDATE SET status=EXCLUDED.status,membership_version=EXCLUDED.membership_version,updated_at=EXCLUDED.updated_at`, organizationID, userID, status, membershipVersion, time.Now().UTC()); err != nil {
		return OrganizationMemberLifecycleState{}, err
	}
	state := OrganizationMemberLifecycleState{OrganizationID: organizationID, UserID: userID, Status: status, MembershipVersion: membershipVersion}
	return state, tx.Commit(ctx)
}
