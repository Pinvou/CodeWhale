package billing

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// These facts belong to the original released hold, not to the grant's current
// aggregate reservation (which may already fund a different ACTIVE hold).
type usageRepairReleasedMember struct {
	Grant          MemberQuotaGrant `json:"grant"`
	Reserved       int64            `json:"original_reserved_microcredits"`
	SettledBefore  int64            `json:"settled_before_microcredits"`
	ReleasedBefore int64            `json:"released_before_microcredits"`
	Settleable     int64            `json:"settleable_microcredits"`
	Snapshot       any              `json:"owner_snapshot"`
}

type usageRepairReleasedMemberIdentity struct {
	OrganizationID    uuid.UUID
	MemberUserID      uuid.UUID
	MembershipVersion int64
	AllocationCount   int64
}

func usageRepairReleasedMemberIdentityFrom(ctx context.Context, source usageRepairReader, holdID, walletID uuid.UUID) (*usageRepairReleasedMemberIdentity, error) {
	var organizationID, memberUserID *uuid.UUID
	var membershipVersion *int64
	var ownerType string
	var ownerID uuid.UUID
	var count int64
	if err := source.QueryRow(ctx, `SELECT hold.organization_id,hold.member_user_id,hold.membership_version,wallet.owner_type,wallet.owner_id,
 (SELECT count(*) FROM member_quota_hold_allocations WHERE hold_id=hold.id)
 FROM usage_holds hold JOIN wallets wallet ON wallet.id=hold.wallet_id WHERE hold.id=$1 AND wallet.id=$2`, holdID, walletID).
		Scan(&organizationID, &memberUserID, &membershipVersion, &ownerType, &ownerID, &count); err != nil {
		return nil, err
	}
	if organizationID == nil && memberUserID == nil && membershipVersion == nil && count == 0 {
		return nil, nil
	}
	if organizationID == nil || memberUserID == nil || membershipVersion == nil || *organizationID == uuid.Nil || *memberUserID == uuid.Nil || *membershipVersion < 1 || count < 1 || ownerType != "ORGANIZATION" || ownerID != *organizationID {
		return nil, ErrApprovalSnapshotMismatch
	}
	return &usageRepairReleasedMemberIdentity{OrganizationID: *organizationID, MemberUserID: *memberUserID, MembershipVersion: *membershipVersion, AllocationCount: count}, nil
}

func usageRepairReleasedMembersFrom(ctx context.Context, source usageRepairReader, holdID uuid.UUID, identity *usageRepairReleasedMemberIdentity, allocation usageRepairAllocation, remaining int64) ([]usageRepairReleasedMember, int64, error) {
	if allocation.ActiveGrant < 0 || allocation.SuspendedGrant < 0 || remaining < 0 {
		return nil, 0, ErrApprovalSnapshotMismatch
	}
	rows, err := source.Query(ctx, `SELECT quota.id,quota.organization_id,quota.member_user_id,quota.package_id,quota.component_id,quota.component_type,
 quota.original_microcredits,quota.available_microcredits,quota.reserved_microcredits,quota.used_microcredits,quota.expired_microcredits,quota.revoked_microcredits,
 quota.status,quota.suspension_reason,quota.membership_version,quota.allocation_version,quota.created_by,quota.expires_at,quota.created_at,quota.updated_at,
 member_allocation.reserved_microcredits,member_allocation.settled_microcredits,member_allocation.released_microcredits,member_allocation.component_id,
 jsonb_build_object('allocation',to_jsonb(member_allocation),'grant',to_jsonb(quota))
 FROM member_quota_hold_allocations member_allocation JOIN member_quota_grants quota ON quota.id=member_allocation.grant_id
 WHERE member_allocation.hold_id=$1 AND member_allocation.component_id=$2 ORDER BY quota.id`, holdID, allocation.ComponentID)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	members := make([]usageRepairReleasedMember, 0)
	originalTotal, settledTotal := int64(0), int64(0)
	activeRemaining, suspendedRemaining := allocation.ActiveGrant, allocation.SuspendedGrant
	for rows.Next() {
		item := usageRepairReleasedMember{}
		grant := &item.Grant
		var memberComponent uuid.UUID
		var snapshot []byte
		if err := rows.Scan(&grant.ID, &grant.OrganizationID, &grant.MemberUserID, &grant.PackageID, &grant.ComponentID, &grant.ComponentType,
			&grant.OriginalMicrocredits, &grant.AvailableMicrocredits, &grant.ReservedMicrocredits, &grant.UsedMicrocredits, &grant.ExpiredMicrocredits, &grant.RevokedMicrocredits,
			&grant.Status, &grant.SuspensionReason, &grant.MembershipVersion, &grant.AllocationVersion, &grant.CreatedBy, &grant.ExpiresAt, &grant.CreatedAt, &grant.UpdatedAt,
			&item.Reserved, &item.SettledBefore, &item.ReleasedBefore, &memberComponent, &snapshot); err != nil {
			return nil, 0, err
		}
		if grant.ID == uuid.Nil || grant.OrganizationID != identity.OrganizationID || grant.MemberUserID != identity.MemberUserID || grant.PackageID != allocation.PackageID || grant.ComponentID != allocation.ComponentID || memberComponent != grant.ComponentID || grant.MembershipVersion < 1 || grant.AllocationVersion < 1 || grant.AvailableMicrocredits < 0 || item.Reserved <= 0 || item.Reserved > allocation.Reserved-originalTotal || item.SettledBefore != 0 || item.ReleasedBefore != item.Reserved {
			return nil, 0, ErrApprovalSnapshotMismatch
		}
		item.Snapshot, err = paymentRepairJSONSnapshot(snapshot)
		if err != nil {
			return nil, 0, ErrApprovalSnapshotMismatch
		}
		capacity := int64(0)
		switch grant.Status {
		case "ACTIVE":
			capacity = min(item.Reserved, min(grant.AvailableMicrocredits, activeRemaining))
		case "SUSPENDED":
			capacity = min(item.Reserved, min(grant.AvailableMicrocredits, suspendedRemaining))
		case "REVOKED", "EXPIRED", "RECLAIMED_ACCOUNT_CLOSED":
			if grant.AvailableMicrocredits != 0 {
				return nil, 0, ErrApprovalSnapshotMismatch
			}
		default:
			return nil, 0, ErrApprovalSnapshotMismatch
		}
		item.Settleable = min(remaining, capacity)
		if grant.Status == "ACTIVE" {
			activeRemaining -= item.Settleable
		} else if grant.Status == "SUSPENDED" {
			suspendedRemaining -= item.Settleable
		}
		remaining -= item.Settleable
		settledTotal += item.Settleable
		originalTotal += item.Reserved
		members = append(members, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if originalTotal != allocation.Reserved || len(members) == 0 {
		return nil, 0, ErrApprovalSnapshotMismatch
	}
	return members, settledTotal, nil
}

// The caller already holds the wallet and component gates. Only grants attached
// to those components and wallet are locked; malformed foreign identities are
// rejected by the following plan, without acquiring another wallet's grant gate.
func lockUsageRepairReleasedMembers(ctx context.Context, tx pgx.Tx, holdID, walletID uuid.UUID) error {
	rows, err := tx.Query(ctx, `SELECT member_allocation.grant_id FROM member_quota_hold_allocations member_allocation
 JOIN member_quota_grants quota ON quota.id=member_allocation.grant_id AND quota.component_id=member_allocation.component_id
 JOIN entitlement_components component ON component.id=quota.component_id AND component.package_id=quota.package_id
 JOIN entitlement_packages package ON package.id=component.package_id AND package.wallet_id=$2
 WHERE member_allocation.hold_id=$1 ORDER BY member_allocation.component_id,member_allocation.grant_id FOR UPDATE OF member_allocation,quota`, holdID, walletID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return err
		}
	}
	return rows.Err()
}

func settleUsageRepairReleasedMembers(ctx context.Context, tx pgx.Tx, holdID uuid.UUID, allocation usageRepairAllocation, now time.Time) error {
	var settledTotal int64
	for _, item := range allocation.Members {
		amount := item.Settleable
		if amount == 0 {
			continue
		}
		if amount < 0 || amount > allocation.Settleable-settledTotal {
			return ErrApprovalSnapshotMismatch
		}
		grant := item.Grant
		bucket := "active_grant_microcredits"
		if grant.Status == "SUSPENDED" {
			bucket = "suspended_grant_microcredits"
		} else if grant.Status != "ACTIVE" {
			return ErrApprovalSnapshotMismatch
		}
		// The reservation has already been returned. Consume that exact current
		// member bucket once, leaving all reserved balances and other holds intact.
		query := `UPDATE entitlement_components SET ` + bucket + `=` + bucket + `-$2,used_microcredits=used_microcredits+$2,
 status=CASE WHEN status IN ('FROZEN','REVOKED','EXPIRED') THEN status
 WHEN available_microcredits=0 AND reserved_microcredits+active_grant_microcredits+suspended_grant_microcredits-$2=0 THEN 'CONSUMED' ELSE 'ACTIVE' END,
 updated_at=$3 WHERE id=$1 AND package_id=$4 AND ` + bucket + `>=$2`
		tag, err := tx.Exec(ctx, query, allocation.ComponentID, amount, now, allocation.PackageID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return usageSettlementRepairConflict("released_member_component_cas", nil)
		}
		tag, err = tx.Exec(ctx, `UPDATE member_quota_grants SET available_microcredits=available_microcredits-$2,used_microcredits=used_microcredits+$2,
 allocation_version=allocation_version+1,updated_at=$3
 WHERE id=$1 AND component_id=$4 AND package_id=$5 AND available_microcredits=$6 AND allocation_version=$7 AND membership_version=$8 AND status=$9`,
			grant.ID, amount, now, allocation.ComponentID, allocation.PackageID, grant.AvailableMicrocredits, grant.AllocationVersion, grant.MembershipVersion, grant.Status)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return usageSettlementRepairConflict("released_member_grant_cas", nil)
		}
		tag, err = tx.Exec(ctx, `UPDATE member_quota_hold_allocations SET settled_microcredits=$3,released_microcredits=$4,updated_at=$5
 WHERE hold_id=$1 AND grant_id=$2 AND component_id=$6 AND reserved_microcredits=$7 AND settled_microcredits=0 AND released_microcredits=$7`,
			holdID, grant.ID, amount, item.Reserved-amount, now, allocation.ComponentID, item.Reserved)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return usageSettlementRepairConflict("released_member_allocation_cas", nil)
		}
		grant.AvailableMicrocredits -= amount
		grant.UsedMicrocredits += amount
		grant.AllocationVersion++
		grant.UpdatedAt = now
		if err := appendMemberQuotaAudit(ctx, tx, grant, "HOLD_SETTLED", nil, amount, now); err != nil {
			return err
		}
		settledTotal += amount
	}
	if settledTotal != allocation.Settleable {
		return ErrApprovalSnapshotMismatch
	}
	return nil
}
