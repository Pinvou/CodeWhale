package billing

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type memberHoldAllocation struct {
	grant    MemberQuotaGrant
	amount   int64
	settled  int64
	released int64
}

func (r *PostgresRepository) ReserveMemberQuota(ctx context.Context, usageRequestID, walletID, organizationID, memberUserID uuid.UUID, membershipVersion, maxAmount int64, priceVersionID *uuid.UUID, expiresAt time.Time) (Hold, error) {
	return r.reserveMemberQuota(ctx, usageRequestID, walletID, organizationID, memberUserID, membershipVersion, maxAmount, priceVersionID, expiresAt, nil)
}

func (r *PostgresRepository) ReserveMemberQuotaPriced(ctx context.Context, usageRequestID, walletID, organizationID, memberUserID uuid.UUID, membershipVersion, maxAmount int64, priceVersionID *uuid.UUID, expiresAt time.Time, pricing *HoldPricingContext) (Hold, error) {
	return r.reserveMemberQuota(ctx, usageRequestID, walletID, organizationID, memberUserID, membershipVersion, maxAmount, priceVersionID, expiresAt, pricing)
}

func (r *PostgresRepository) reserveMemberQuota(ctx context.Context, usageRequestID, walletID, organizationID, memberUserID uuid.UUID, membershipVersion, maxAmount int64, priceVersionID *uuid.UUID, expiresAt time.Time, pricing *HoldPricingContext) (Hold, error) {
	if organizationID == uuid.Nil || memberUserID == uuid.Nil || membershipVersion < 1 || maxAmount < 0 || !expiresAt.After(r.now()) {
		return Hold{}, ErrConflict
	}
	if pricing != nil && (strings.TrimSpace(pricing.ModelID) == "" || strings.TrimSpace(pricing.ProtocolCode) == "" || pricing.FormulaVersionID == uuid.Nil || !json.Valid(pricing.PricingSnapshot) || !json.Valid(pricing.RequestSnapshot)) {
		return Hold{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Hold{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "usage.reserve", usageRequestID.String()); err != nil {
		return Hold{}, err
	}
	existing, err := getHoldByUsage(ctx, tx, usageRequestID, true)
	if err == nil {
		if existing.WalletID != walletID || existing.MaxMicrocredits != maxAmount || !equalUUID(existing.PriceVersionID, priceVersionID) {
			return Hold{}, ErrConflict
		}
		return existing, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Hold{}, err
	}
	var lifecycleStatus string
	var lifecycleVersion int64
	if err := tx.QueryRow(ctx, `SELECT status,membership_version FROM organization_member_lifecycle_states WHERE organization_id=$1 AND user_id=$2 FOR UPDATE`, organizationID, memberUserID).Scan(&lifecycleStatus, &lifecycleVersion); err != nil || lifecycleStatus != "ACTIVE" || lifecycleVersion != membershipVersion {
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return Hold{}, err
		}
		return Hold{}, ErrConflict
	}
	wallet, err := getWallet(ctx, tx, walletID, true)
	if err != nil {
		return Hold{}, ErrNotFound
	}
	if wallet.Status != "ACTIVE" || wallet.OwnerType != "ORGANIZATION" || wallet.OwnerID != organizationID {
		return Hold{}, ErrWalletUnavailable
	}
	if wallet.AvailableMicrocredits < maxAmount {
		return Hold{}, ErrInsufficientBalance
	}
	now := r.now()
	rows, err := tx.Query(ctx, `SELECT grant.id,grant.organization_id,grant.member_user_id,grant.package_id,grant.component_id,grant.component_type,grant.original_microcredits,grant.available_microcredits,grant.reserved_microcredits,grant.used_microcredits,grant.expired_microcredits,grant.revoked_microcredits,grant.status,grant.suspension_reason,grant.membership_version,grant.allocation_version,grant.created_by,grant.expires_at,grant.created_at,grant.updated_at
FROM member_quota_grants grant
JOIN entitlement_components component ON component.id=grant.component_id
JOIN entitlement_packages package ON package.id=grant.package_id
WHERE grant.organization_id=$1 AND grant.member_user_id=$2 AND grant.status='ACTIVE' AND grant.membership_version=$3 AND grant.expires_at>$4 AND grant.available_microcredits>0 AND component.status='ACTIVE' AND package.status='ACTIVE'
ORDER BY grant.expires_at,CASE grant.component_type WHEN 'BONUS' THEN 0 ELSE 1 END,grant.component_id,grant.id
FOR UPDATE OF grant,component`, organizationID, memberUserID, membershipVersion, now)
	if err != nil {
		return Hold{}, err
	}
	remaining := maxAmount
	allocations := make([]memberHoldAllocation, 0)
	for rows.Next() {
		var grant MemberQuotaGrant
		if err := scanMemberQuotaGrant(rows, &grant); err != nil {
			rows.Close()
			return Hold{}, err
		}
		amount := min(remaining, grant.AvailableMicrocredits)
		if amount > 0 {
			allocations = append(allocations, memberHoldAllocation{grant: grant, amount: amount})
			remaining -= amount
		}
		if remaining == 0 {
			break
		}
	}
	rows.Close()
	if remaining != 0 {
		return Hold{}, ErrInsufficientComponentCapacity
	}
	command, err := tx.Exec(ctx, `UPDATE wallets SET reserved_microcredits=reserved_microcredits+$2,version=version+1,updated_at=$3 WHERE id=$1 AND status='ACTIVE' AND posted_microcredits-reserved_microcredits>=$2`, walletID, maxAmount, now)
	if err != nil || command.RowsAffected() != 1 {
		if err == nil {
			err = ErrInsufficientBalance
		}
		return Hold{}, err
	}
	hold := Hold{ID: uuid.Must(uuid.NewV7()), UsageRequestID: usageRequestID, WalletID: walletID, PriceVersionID: priceVersionID, Status: "ACTIVE", MaxMicrocredits: maxAmount, ExpiresAt: expiresAt.UTC()}
	var modelID, protocolCode any
	var formulaVersionID any
	var pricingSnapshot, requestSnapshot any
	if pricing != nil {
		modelID, protocolCode, formulaVersionID = pricing.ModelID, pricing.ProtocolCode, pricing.FormulaVersionID
		pricingSnapshot, requestSnapshot = []byte(pricing.PricingSnapshot), []byte(pricing.RequestSnapshot)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO usage_holds(id,usage_request_id,wallet_id,price_version_id,status,max_microcredits,expires_at,model_id,protocol_code,formula_version_id,pricing_snapshot,request_snapshot,organization_id,member_user_id,membership_version,created_at,updated_at) VALUES($1,$2,$3,$4,'ACTIVE',$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$15)`, hold.ID, usageRequestID, walletID, priceVersionID, maxAmount, hold.ExpiresAt, modelID, protocolCode, formulaVersionID, pricingSnapshot, requestSnapshot, organizationID, memberUserID, membershipVersion, now); err != nil {
		return Hold{}, err
	}
	componentAmounts := make(map[uuid.UUID]int64)
	componentPackages := make(map[uuid.UUID]uuid.UUID)
	for _, allocation := range allocations {
		command, err := tx.Exec(ctx, `UPDATE member_quota_grants SET available_microcredits=available_microcredits-$2,reserved_microcredits=reserved_microcredits+$2,allocation_version=allocation_version+1,updated_at=$3 WHERE id=$1 AND status='ACTIVE' AND available_microcredits>=$2`, allocation.grant.ID, allocation.amount, now)
		if err != nil || command.RowsAffected() != 1 {
			if err == nil {
				err = ErrConflict
			}
			return Hold{}, err
		}
		command, err = tx.Exec(ctx, `UPDATE entitlement_components SET active_grant_microcredits=active_grant_microcredits-$2,reserved_microcredits=reserved_microcredits+$2,updated_at=$3 WHERE id=$1 AND active_grant_microcredits>=$2`, allocation.grant.ComponentID, allocation.amount, now)
		if err != nil || command.RowsAffected() != 1 {
			if err == nil {
				err = ErrConflict
			}
			return Hold{}, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO member_quota_hold_allocations(hold_id,grant_id,component_id,reserved_microcredits,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$5)`, hold.ID, allocation.grant.ID, allocation.grant.ComponentID, allocation.amount, now); err != nil {
			return Hold{}, err
		}
		componentAmounts[allocation.grant.ComponentID] += allocation.amount
		componentPackages[allocation.grant.ComponentID] = allocation.grant.PackageID
		allocation.grant.AvailableMicrocredits -= allocation.amount
		allocation.grant.ReservedMicrocredits += allocation.amount
		allocation.grant.AllocationVersion++
		if err := appendMemberQuotaAudit(ctx, tx, allocation.grant, "HOLD_RESERVED", nil, allocation.amount, now); err != nil {
			return Hold{}, err
		}
	}
	for componentID, amount := range componentAmounts {
		if _, err := tx.Exec(ctx, `INSERT INTO usage_hold_allocations(hold_id,package_id,component_id,reserved_microcredits,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$5)`, hold.ID, componentPackages[componentID], componentID, amount, now); err != nil {
			return Hold{}, err
		}
	}
	return hold, tx.Commit(ctx)
}

func hasMemberQuotaAllocations(ctx context.Context, tx pgx.Tx, holdID, componentID uuid.UUID) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM member_quota_hold_allocations WHERE hold_id=$1 AND component_id=$2)`, holdID, componentID).Scan(&exists)
	return exists, err
}

func (r *PostgresRepository) extendMemberQuotaHold(ctx context.Context, tx pgx.Tx, hold Hold, organizationID, memberUserID uuid.UUID, membershipVersion, additional int64, now time.Time) error {
	var lifecycleStatus string
	var lifecycleVersion int64
	if err := tx.QueryRow(ctx, `SELECT status,membership_version FROM organization_member_lifecycle_states WHERE organization_id=$1 AND user_id=$2 FOR UPDATE`, organizationID, memberUserID).Scan(&lifecycleStatus, &lifecycleVersion); err != nil || lifecycleStatus != "ACTIVE" || lifecycleVersion != membershipVersion {
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return ErrConflict
	}
	wallet, err := getWallet(ctx, tx, hold.WalletID, true)
	if err != nil {
		return err
	}
	if wallet.Status != "ACTIVE" || wallet.OwnerType != "ORGANIZATION" || wallet.OwnerID != organizationID {
		return ErrWalletUnavailable
	}
	if wallet.AvailableMicrocredits < additional {
		return ErrInsufficientBalance
	}
	rows, err := tx.Query(ctx, `SELECT grant.id,grant.organization_id,grant.member_user_id,grant.package_id,grant.component_id,grant.component_type,grant.original_microcredits,grant.available_microcredits,grant.reserved_microcredits,grant.used_microcredits,grant.expired_microcredits,grant.revoked_microcredits,grant.status,grant.suspension_reason,grant.membership_version,grant.allocation_version,grant.created_by,grant.expires_at,grant.created_at,grant.updated_at FROM member_quota_grants grant JOIN entitlement_components component ON component.id=grant.component_id JOIN entitlement_packages package ON package.id=grant.package_id WHERE grant.organization_id=$1 AND grant.member_user_id=$2 AND grant.status='ACTIVE' AND grant.membership_version=$3 AND grant.expires_at>$4 AND grant.available_microcredits>0 AND component.status='ACTIVE' AND package.status='ACTIVE' ORDER BY grant.expires_at,CASE grant.component_type WHEN 'BONUS' THEN 0 ELSE 1 END,grant.component_id,grant.id FOR UPDATE OF grant,component`, organizationID, memberUserID, membershipVersion, now)
	if err != nil {
		return err
	}
	remaining := additional
	items := make([]memberHoldAllocation, 0)
	for rows.Next() {
		var grant MemberQuotaGrant
		if err := scanMemberQuotaGrant(rows, &grant); err != nil {
			rows.Close()
			return err
		}
		amount := min(remaining, grant.AvailableMicrocredits)
		if amount > 0 {
			items = append(items, memberHoldAllocation{grant: grant, amount: amount})
			remaining -= amount
		}
		if remaining == 0 {
			break
		}
	}
	rows.Close()
	if remaining != 0 {
		return ErrInsufficientComponentCapacity
	}
	command, err := tx.Exec(ctx, `UPDATE wallets SET reserved_microcredits=reserved_microcredits+$2,version=version+1,updated_at=$3 WHERE id=$1 AND status='ACTIVE' AND posted_microcredits-reserved_microcredits>=$2`, hold.WalletID, additional, now)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrInsufficientBalance
	}
	componentAmounts := map[uuid.UUID]int64{}
	componentPackages := map[uuid.UUID]uuid.UUID{}
	for _, item := range items {
		command, err = tx.Exec(ctx, `UPDATE member_quota_grants SET available_microcredits=available_microcredits-$2,reserved_microcredits=reserved_microcredits+$2,allocation_version=allocation_version+1,updated_at=$3 WHERE id=$1 AND status='ACTIVE' AND available_microcredits>=$2`, item.grant.ID, item.amount, now)
		if err != nil || command.RowsAffected() != 1 {
			if err != nil {
				return err
			}
			return ErrConflict
		}
		command, err = tx.Exec(ctx, `UPDATE entitlement_components SET active_grant_microcredits=active_grant_microcredits-$2,reserved_microcredits=reserved_microcredits+$2,updated_at=$3 WHERE id=$1 AND active_grant_microcredits>=$2`, item.grant.ComponentID, item.amount, now)
		if err != nil || command.RowsAffected() != 1 {
			if err != nil {
				return err
			}
			return ErrConflict
		}
		_, err = tx.Exec(ctx, `INSERT INTO member_quota_hold_allocations(hold_id,grant_id,component_id,reserved_microcredits,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$5) ON CONFLICT(hold_id,grant_id) DO UPDATE SET reserved_microcredits=member_quota_hold_allocations.reserved_microcredits+EXCLUDED.reserved_microcredits,updated_at=EXCLUDED.updated_at`, hold.ID, item.grant.ID, item.grant.ComponentID, item.amount, now)
		if err != nil {
			return err
		}
		componentAmounts[item.grant.ComponentID] += item.amount
		componentPackages[item.grant.ComponentID] = item.grant.PackageID
		item.grant.AvailableMicrocredits -= item.amount
		item.grant.ReservedMicrocredits += item.amount
		item.grant.AllocationVersion++
		if err := appendMemberQuotaAudit(ctx, tx, item.grant, "HOLD_RESERVED", nil, item.amount, now); err != nil {
			return err
		}
	}
	for componentID, amount := range componentAmounts {
		_, err = tx.Exec(ctx, `INSERT INTO usage_hold_allocations(hold_id,package_id,component_id,reserved_microcredits,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$5) ON CONFLICT(hold_id,component_id) DO UPDATE SET reserved_microcredits=usage_hold_allocations.reserved_microcredits+EXCLUDED.reserved_microcredits,updated_at=EXCLUDED.updated_at`, hold.ID, componentPackages[componentID], componentID, amount, now)
		if err != nil {
			return err
		}
	}
	return nil
}

func settleMemberQuotaComponent(ctx context.Context, tx pgx.Tx, holdID, componentID uuid.UUID, settledTotal int64, now time.Time) error {
	rows, err := tx.Query(ctx, `SELECT grant.id,grant.organization_id,grant.member_user_id,grant.package_id,grant.component_id,grant.component_type,grant.original_microcredits,grant.available_microcredits,grant.reserved_microcredits,grant.used_microcredits,grant.expired_microcredits,grant.revoked_microcredits,grant.status,grant.suspension_reason,grant.membership_version,grant.allocation_version,grant.created_by,grant.expires_at,grant.created_at,grant.updated_at,allocation.reserved_microcredits
FROM member_quota_hold_allocations allocation JOIN member_quota_grants grant ON grant.id=allocation.grant_id
WHERE allocation.hold_id=$1 AND allocation.component_id=$2 ORDER BY grant.id FOR UPDATE OF allocation,grant`, holdID, componentID)
	if err != nil {
		return err
	}
	items := make([]memberHoldAllocation, 0)
	remaining := settledTotal
	for rows.Next() {
		var item memberHoldAllocation
		if err := scanMemberQuotaGrantWithAmount(rows, &item.grant, &item.amount); err != nil {
			rows.Close()
			return err
		}
		item.settled = min(remaining, item.amount)
		item.released = item.amount - item.settled
		remaining -= item.settled
		items = append(items, item)
	}
	rows.Close()
	if remaining != 0 {
		return ErrConflict
	}
	for _, item := range items {
		bucket := "active_grant_microcredits"
		if item.grant.Status == "SUSPENDED" {
			bucket = "suspended_grant_microcredits"
		}
		query := `UPDATE entitlement_components SET reserved_microcredits=reserved_microcredits-$2,used_microcredits=used_microcredits+$3,` + bucket + `=` + bucket + `+$4,updated_at=$5 WHERE id=$1 AND reserved_microcredits>=$2`
		command, err := tx.Exec(ctx, query, componentID, item.amount, item.settled, item.released, now)
		if err != nil || command.RowsAffected() != 1 {
			if err == nil {
				err = ErrConflict
			}
			return err
		}
		command, err = tx.Exec(ctx, `UPDATE member_quota_grants SET reserved_microcredits=reserved_microcredits-$2,used_microcredits=used_microcredits+$3,available_microcredits=available_microcredits+$4,allocation_version=allocation_version+1,updated_at=$5 WHERE id=$1 AND reserved_microcredits>=$2`, item.grant.ID, item.amount, item.settled, item.released, now)
		if err != nil || command.RowsAffected() != 1 {
			if err == nil {
				err = ErrConflict
			}
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE member_quota_hold_allocations SET settled_microcredits=$3,released_microcredits=$4,updated_at=$5 WHERE hold_id=$1 AND grant_id=$2`, holdID, item.grant.ID, item.settled, item.released, now); err != nil {
			return err
		}
		item.grant.ReservedMicrocredits -= item.amount
		item.grant.UsedMicrocredits += item.settled
		item.grant.AvailableMicrocredits += item.released
		item.grant.AllocationVersion++
		if err := appendMemberQuotaAudit(ctx, tx, item.grant, "HOLD_SETTLED", nil, item.settled, now); err != nil {
			return err
		}
		if item.released > 0 {
			if err := appendMemberQuotaAudit(ctx, tx, item.grant, "HOLD_RELEASED", nil, item.released, now); err != nil {
				return err
			}
		}
	}
	return nil
}

func scanMemberQuotaGrantWithAmount(scanner grantScanner, grant *MemberQuotaGrant, amount *int64) error {
	return scanner.Scan(&grant.ID, &grant.OrganizationID, &grant.MemberUserID, &grant.PackageID, &grant.ComponentID, &grant.ComponentType, &grant.OriginalMicrocredits, &grant.AvailableMicrocredits, &grant.ReservedMicrocredits, &grant.UsedMicrocredits, &grant.ExpiredMicrocredits, &grant.RevokedMicrocredits, &grant.Status, &grant.SuspensionReason, &grant.MembershipVersion, &grant.AllocationVersion, &grant.CreatedBy, &grant.ExpiresAt, &grant.CreatedAt, &grant.UpdatedAt, amount)
}
