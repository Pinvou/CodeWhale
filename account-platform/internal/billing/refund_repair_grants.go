package billing

import (
	"context"

	"github.com/google/uuid"
)

// A repair only revokes the exact unconsumed grants of components that its
// frozen plan actually reclaims. Other components become shortfall evidence;
// their grants and membership/account-closure facts remain unchanged.
type refundRepairGrant struct {
	ID                uuid.UUID
	ComponentID       uuid.UUID
	Available         int64
	Status            string
	SuspensionReason  *string
	AllocationVersion int64
}

func refundRepairGrantStateEligible(prior string, priorReason *string, current string, currentReason *string) bool {
	if current != "SUSPENDED" || currentReason == nil {
		return false
	}
	if prior == "ACTIVE" {
		return priorReason == nil && *currentReason == "REFUND_PENDING"
	}
	return prior == "SUSPENDED" && priorReason != nil &&
		(*priorReason == "MEMBER_REMOVED" || *priorReason == "ACCOUNT_CLOSED") && *currentReason == *priorReason
}

func refundRepairGrantsFrom(ctx context.Context, source refundExceptionRepairReader, refundID, packageID uuid.UUID, components []refundExceptionReclaim) ([]refundRepairGrant, error) {
	reclaimable := make(map[uuid.UUID]bool, len(components))
	for _, component := range components {
		reclaimable[component.ComponentID] = component.Reclaimable
	}
	rows, err := source.Query(ctx, `SELECT reservation.grant_id,reservation.component_id,reservation.available_microcredits,
 reservation.prior_status,reservation.prior_suspension_reason,quota_grant.package_id,quota_grant.component_id,
 quota_grant.available_microcredits,quota_grant.status,quota_grant.suspension_reason,quota_grant.allocation_version,
 quota_grant.used_microcredits,quota_grant.reserved_microcredits,quota_grant.expired_microcredits,quota_grant.revoked_microcredits
 FROM refund_grant_reservations reservation JOIN member_quota_grants quota_grant ON quota_grant.id=reservation.grant_id
 WHERE reservation.refund_id=$1 ORDER BY reservation.grant_id`, refundID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	grants := make([]refundRepairGrant, 0)
	for rows.Next() {
		var grant refundRepairGrant
		var reservedComponent, currentPackage uuid.UUID
		var priorStatus string
		var priorReason *string
		var originalAvailable, used, reserved, expired, revoked int64
		if err := rows.Scan(&grant.ID, &reservedComponent, &originalAvailable, &priorStatus, &priorReason,
			&currentPackage, &grant.ComponentID, &grant.Available, &grant.Status, &grant.SuspensionReason,
			&grant.AllocationVersion, &used, &reserved, &expired, &revoked); err != nil {
			return nil, err
		}
		if currentPackage != packageID || grant.ComponentID != reservedComponent {
			return nil, ErrApprovalSnapshotMismatch
		}
		canReclaim, knownComponent := reclaimable[reservedComponent]
		if !knownComponent {
			return nil, ErrApprovalSnapshotMismatch
		}
		if !canReclaim {
			continue
		}
		if grant.Available != originalAvailable || grant.Available < 0 || used != 0 || reserved != 0 || expired != 0 || revoked != 0 ||
			!refundRepairGrantStateEligible(priorStatus, priorReason, grant.Status, grant.SuspensionReason) {
			return nil, ErrApprovalSnapshotMismatch
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}
