package billing

import (
	"context"
	"math"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type memberHoldIdentity struct {
	walletID, walletOwnerID      uuid.UUID
	walletOwnerType              string
	organizationID, memberUserID *uuid.UUID
	membershipVersion            *int64
	maximum                      int64
}

type memberHoldComponent struct {
	packageID, componentID, componentPackageID, walletID uuid.UUID
	reserved, settled, released                          int64
}

type memberHoldGrantAllocation struct {
	organizationID, memberUserID                                           uuid.UUID
	packageID, componentID, grantComponentID, componentPackageID, walletID uuid.UUID
	componentType, grantComponentType                                      string
	reserved, settled, released                                            int64
}

// Callers already hold the wallet, then the hold lock. Validate reserved facts
// before changing buckets, without re-authorizing an in-flight hold against
// a newer membership version, lifecycle status, or expiry.
func validateMemberQuotaHoldAllocations(ctx context.Context, tx pgx.Tx, holdID uuid.UUID, additional int64) (bool, error) {
	var identity memberHoldIdentity
	if err := tx.QueryRow(ctx, `SELECT h.wallet_id,w.owner_type,w.owner_id,h.organization_id,h.member_user_id,h.membership_version,h.max_microcredits FROM usage_holds h JOIN wallets w ON w.id=h.wallet_id WHERE h.id=$1`, holdID).Scan(&identity.walletID, &identity.walletOwnerType, &identity.walletOwnerID, &identity.organizationID, &identity.memberUserID, &identity.membershipVersion, &identity.maximum); err != nil {
		return false, err
	}
	rows, err := tx.Query(ctx, `SELECT q.organization_id,q.member_user_id,q.package_id,a.component_id,q.component_id,c.package_id,p.wallet_id,c.component_type,q.component_type,a.reserved_microcredits,a.settled_microcredits,a.released_microcredits FROM member_quota_hold_allocations a JOIN member_quota_grants q ON q.id=a.grant_id JOIN entitlement_components c ON c.id=a.component_id JOIN entitlement_packages p ON p.id=c.package_id WHERE a.hold_id=$1`, holdID)
	if err != nil {
		return false, err
	}
	grants := make([]memberHoldGrantAllocation, 0)
	for rows.Next() {
		var a memberHoldGrantAllocation
		if err := rows.Scan(&a.organizationID, &a.memberUserID, &a.packageID, &a.componentID, &a.grantComponentID, &a.componentPackageID, &a.walletID, &a.componentType, &a.grantComponentType, &a.reserved, &a.settled, &a.released); err != nil {
			rows.Close()
			return false, err
		}
		grants = append(grants, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	// Generic holds, including organization wallets without a member scope,
	// retain their existing behavior.
	if identity.organizationID == nil && identity.memberUserID == nil && identity.membershipVersion == nil && len(grants) == 0 {
		return false, nil
	}
	rows, err = tx.Query(ctx, `SELECT a.package_id,a.component_id,c.package_id,p.wallet_id,a.reserved_microcredits,a.settled_microcredits,a.released_microcredits FROM usage_hold_allocations a JOIN entitlement_components c ON c.id=a.component_id JOIN entitlement_packages p ON p.id=c.package_id WHERE a.hold_id=$1`, holdID)
	if err != nil {
		return false, err
	}
	components := make([]memberHoldComponent, 0)
	for rows.Next() {
		var a memberHoldComponent
		if err := rows.Scan(&a.packageID, &a.componentID, &a.componentPackageID, &a.walletID, &a.reserved, &a.settled, &a.released); err != nil {
			rows.Close()
			return false, err
		}
		components = append(components, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	return true, checkMemberHoldIdentity(identity, components, grants, additional)
}

func checkMemberHoldIdentity(h memberHoldIdentity, components []memberHoldComponent, grants []memberHoldGrantAllocation, additional int64) error {
	if h.organizationID == nil || h.memberUserID == nil || h.membershipVersion == nil || *h.organizationID == uuid.Nil || *h.memberUserID == uuid.Nil || *h.membershipVersion < 1 || h.walletOwnerType != "ORGANIZATION" || h.walletOwnerID != *h.organizationID || h.maximum < 0 || additional < 0 || h.maximum > math.MaxInt64-additional {
		return ErrConflict
	}
	remaining := h.maximum + additional
	budgets := make(map[uuid.UUID]memberHoldComponent, len(components))
	for _, c := range components {
		if c.walletID != h.walletID || c.packageID != c.componentPackageID || c.reserved <= 0 || c.settled != 0 || c.released != 0 || c.reserved > remaining {
			return ErrConflict
		}
		if _, duplicate := budgets[c.componentID]; duplicate {
			return ErrConflict
		}
		budgets[c.componentID] = c
		remaining -= c.reserved
	}
	if remaining != 0 {
		return ErrConflict
	}
	for _, a := range grants {
		c, exists := budgets[a.componentID]
		if !exists || a.organizationID != *h.organizationID || a.memberUserID != *h.memberUserID || a.componentID != a.grantComponentID || a.packageID != a.componentPackageID || a.packageID != c.packageID || a.walletID != h.walletID || a.componentType != a.grantComponentType || a.reserved <= 0 || a.reserved > c.reserved || a.settled != 0 || a.released != 0 {
			return ErrConflict
		}
		c.reserved -= a.reserved
		budgets[a.componentID] = c
	}
	for _, c := range budgets {
		if c.reserved != 0 {
			return ErrConflict
		}
	}
	return nil
}
