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
	if organizationID == uuid.Nil || userID == uuid.Nil || membershipVersion < 1 || (status != "ACTIVE" && status != "REVOKED" && status != "SUSPENDED") {
		return OrganizationMemberLifecycleState{}, ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return OrganizationMemberLifecycleState{}, err
	}
	defer tx.Rollback(ctx)
	state, err := r.applyOrganizationMemberLifecycle(ctx, tx, organizationID, userID, status, membershipVersion, false)
	if err != nil {
		return OrganizationMemberLifecycleState{}, err
	}
	return state, tx.Commit(ctx)
}

// HTTP writes and event projection use one transaction for lifecycle state,
// quota capacity, audits and (for events) the Inbox receipt.
func (r *PostgresRepository) applyOrganizationMemberLifecycle(ctx context.Context, tx pgx.Tx, organizationID, userID uuid.UUID, status string, membershipVersion int64, ignoreStale bool) (OrganizationMemberLifecycleState, error) {
	if _, err := lockMemberQuotaWallet(ctx, tx, organizationID); err != nil && !errors.Is(err, ErrNotFound) {
		return OrganizationMemberLifecycleState{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "organization-member:"+organizationID.String()+":"+userID.String()); err != nil {
		return OrganizationMemberLifecycleState{}, err
	}
	var currentStatus string
	var currentVersion int64
	err := tx.QueryRow(ctx, `SELECT status,membership_version FROM organization_member_lifecycle_states WHERE organization_id=$1 AND user_id=$2 FOR UPDATE`, organizationID, userID).Scan(&currentStatus, &currentVersion)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return OrganizationMemberLifecycleState{}, err
	}
	if err == nil && (membershipVersion < currentVersion || (membershipVersion == currentVersion && status != currentStatus)) {
		if ignoreStale && membershipVersion < currentVersion {
			return OrganizationMemberLifecycleState{OrganizationID: organizationID, UserID: userID, Status: currentStatus, MembershipVersion: currentVersion}, nil
		}
		return OrganizationMemberLifecycleState{}, ErrConflict
	}
	changed := errors.Is(err, pgx.ErrNoRows) || status != currentStatus || membershipVersion != currentVersion
	now := r.now()
	if _, err := tx.Exec(ctx, `INSERT INTO organization_member_lifecycle_states(organization_id,user_id,status,membership_version,updated_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT(organization_id,user_id) DO UPDATE SET status=EXCLUDED.status,membership_version=EXCLUDED.membership_version,updated_at=EXCLUDED.updated_at`, organizationID, userID, status, membershipVersion, now); err != nil {
		return OrganizationMemberLifecycleState{}, err
	}
	if changed {
		if err := r.applyMemberQuotaLifecycle(ctx, tx, organizationID, userID, status, membershipVersion, now); err != nil {
			return OrganizationMemberLifecycleState{}, err
		}
	}
	state := OrganizationMemberLifecycleState{OrganizationID: organizationID, UserID: userID, Status: status, MembershipVersion: membershipVersion}
	return state, nil
}

func (r *PostgresRepository) applyMemberQuotaLifecycle(ctx context.Context, tx pgx.Tx, organizationID, userID uuid.UUID, lifecycleStatus string, membershipVersion int64, now time.Time) error {
	wantedStatus := "ACTIVE"
	if lifecycleStatus != "ACTIVE" {
		wantedStatus = "SUSPENDED"
	}
	rows, err := tx.Query(ctx, `SELECT id FROM member_quota_grants
WHERE organization_id=$1 AND member_user_id=$2
  AND (($3='SUSPENDED' AND (status='ACTIVE' OR (status='SUSPENDED' AND suspension_reason='MEMBER_REMOVED'))) OR ($3='ACTIVE' AND (status='ACTIVE' OR (status='SUSPENDED' AND suspension_reason='MEMBER_REMOVED'))))
ORDER BY component_id,id FOR UPDATE`, organizationID, userID, wantedStatus)
	if err != nil {
		return err
	}
	grantIDs := make([]uuid.UUID, 0)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		grantIDs = append(grantIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, grantID := range grantIDs {
		grant, _, err := loadMemberQuotaGrant(ctx, tx, grantID, false)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM entitlement_components WHERE id=$1 FOR UPDATE`, grant.ComponentID); err != nil {
			return err
		}
		amount := grant.AvailableMicrocredits
		action, eventType := "SUSPENDED", "billing.member-quota.suspended.v1"
		if wantedStatus == grant.Status {
			_, err = tx.Exec(ctx, `UPDATE member_quota_grants SET membership_version=$2,allocation_version=allocation_version+1,updated_at=$3 WHERE id=$1`, grant.ID, membershipVersion, now)
			grant.MembershipVersion = membershipVersion
			action, eventType = "MEMBERSHIP_UPDATED", "billing.member-quota.membership-updated.v1"
		} else if wantedStatus == "SUSPENDED" {
			if amount > 0 {
				command, err := tx.Exec(ctx, `UPDATE entitlement_components SET active_grant_microcredits=active_grant_microcredits-$2,suspended_grant_microcredits=suspended_grant_microcredits+$2,updated_at=$3 WHERE id=$1 AND active_grant_microcredits>=$2`, grant.ComponentID, amount, now)
				if err != nil || command.RowsAffected() != 1 {
					if err == nil {
						err = ErrConflict
					}
					return err
				}
			}
			_, err = tx.Exec(ctx, `UPDATE member_quota_grants SET status='SUSPENDED',suspension_reason='MEMBER_REMOVED',membership_version=$2,allocation_version=allocation_version+1,updated_at=$3 WHERE id=$1`, grant.ID, membershipVersion, now)
			grant.Status, grant.MembershipVersion = "SUSPENDED", membershipVersion
			reason := "MEMBER_REMOVED"
			grant.SuspensionReason = &reason
		} else {
			if amount > 0 {
				command, err := tx.Exec(ctx, `UPDATE entitlement_components SET suspended_grant_microcredits=suspended_grant_microcredits-$2,active_grant_microcredits=active_grant_microcredits+$2,updated_at=$3 WHERE id=$1 AND suspended_grant_microcredits>=$2`, grant.ComponentID, amount, now)
				if err != nil || command.RowsAffected() != 1 {
					if err == nil {
						err = ErrConflict
					}
					return err
				}
			}
			_, err = tx.Exec(ctx, `UPDATE member_quota_grants SET status='ACTIVE',suspension_reason=NULL,membership_version=$2,allocation_version=allocation_version+1,updated_at=$3 WHERE id=$1`, grant.ID, membershipVersion, now)
			grant.Status, grant.SuspensionReason, grant.MembershipVersion = "ACTIVE", nil, membershipVersion
			action, eventType = "RESTORED", "billing.member-quota.restored.v1"
		}
		if err != nil {
			return err
		}
		grant.AllocationVersion++
		grant.UpdatedAt = now
		if err := appendMemberQuotaAudit(ctx, tx, grant, action, nil, amount, now); err != nil {
			return err
		}
		if err := appendMemberQuotaOutbox(ctx, tx, grant, eventType, now); err != nil {
			return err
		}
	}
	return nil
}
