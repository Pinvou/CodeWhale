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

func (r *PostgresRepository) Reserve(ctx context.Context, usageRequestID, walletID uuid.UUID, maxAmount int64, priceVersionID *uuid.UUID, expiresAt time.Time) (Hold, error) {
	return r.reserve(ctx, usageRequestID, walletID, maxAmount, priceVersionID, expiresAt, nil)
}

func (r *PostgresRepository) ReservePriced(ctx context.Context, usageRequestID, walletID uuid.UUID, maxAmount int64, priceVersionID *uuid.UUID, expiresAt time.Time, pricing *HoldPricingContext) (Hold, error) {
	return r.reserve(ctx, usageRequestID, walletID, maxAmount, priceVersionID, expiresAt, pricing)
}

func (r *PostgresRepository) reserve(ctx context.Context, usageRequestID, walletID uuid.UUID, maxAmount int64, priceVersionID *uuid.UUID, expiresAt time.Time, pricing *HoldPricingContext) (Hold, error) {
	if maxAmount < 0 || !expiresAt.After(r.now()) {
		return Hold{}, ErrConflict
	}
	if pricing != nil && (strings.TrimSpace(pricing.ModelID) == "" || strings.TrimSpace(pricing.ProtocolCode) == "" || pricing.FormulaVersionID == uuid.Nil || !json.Valid(pricing.PricingSnapshot) || !json.Valid(pricing.RequestSnapshot)) {
		return Hold{}, ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
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
		if same, err := sameHoldPricing(ctx, tx, existing.ID, pricing); err != nil {
			return Hold{}, err
		} else if !same {
			return Hold{}, ErrConflict
		}
		return existing, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Hold{}, err
	}
	wallet, err := getWallet(ctx, tx, walletID, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return Hold{}, ErrNotFound
	}
	if err != nil {
		return Hold{}, err
	}
	now := r.now()
	if !expiresAt.After(now) {
		return Hold{}, ErrConflict
	}
	if wallet.OwnerType != "PERSONAL" {
		return Hold{}, ErrConflict
	}
	if wallet.Status != "ACTIVE" {
		return Hold{}, ErrWalletUnavailable
	}
	if wallet.AvailableMicrocredits < maxAmount {
		return Hold{}, ErrInsufficientBalance
	}
	type packageAllocation struct {
		packageID   uuid.UUID
		componentID uuid.UUID
		amount      int64
	}
	rows, err := tx.Query(ctx, `SELECT component.package_id,component.id,component.available_microcredits FROM entitlement_components component JOIN entitlement_packages package ON package.id=component.package_id WHERE package.wallet_id=$1 AND package.status='ACTIVE' AND package.valid_from<=$2 AND component.expires_at>$2 AND component.status='ACTIVE' AND component.available_microcredits>0 ORDER BY component.expires_at,CASE component.component_type WHEN 'BONUS' THEN 0 ELSE 1 END,component.credited_at,component.id FOR UPDATE OF component`, walletID, now)
	if err != nil {
		return Hold{}, err
	}
	remaining := maxAmount
	allocations := make([]packageAllocation, 0)
	for rows.Next() {
		var packageID, componentID uuid.UUID
		var available int64
		if err := rows.Scan(&packageID, &componentID, &available); err != nil {
			rows.Close()
			return Hold{}, err
		}
		amount := min(remaining, available)
		if amount > 0 {
			allocations = append(allocations, packageAllocation{packageID: packageID, componentID: componentID, amount: amount})
			remaining -= amount
		}
		if remaining == 0 {
			break
		}
	}
	rows.Close()
	if remaining != 0 {
		return Hold{}, ErrInsufficientBalance
	}
	command, err := tx.Exec(ctx, `
UPDATE wallets SET reserved_microcredits = reserved_microcredits + $2, version = version + 1, updated_at = $3
WHERE id = $1 AND status = 'ACTIVE' AND receivable_microcredits=0 AND posted_microcredits - reserved_microcredits >= $2`, walletID, maxAmount, now)
	if err != nil {
		return Hold{}, err
	}
	if command.RowsAffected() != 1 {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM wallets WHERE id = $1)`, walletID).Scan(&exists); err != nil {
			return Hold{}, err
		}
		if !exists {
			return Hold{}, ErrNotFound
		}
		return Hold{}, ErrInsufficientBalance
	}
	hold := Hold{ID: uuid.Must(uuid.NewV7()), UsageRequestID: usageRequestID, WalletID: walletID, PriceVersionID: priceVersionID, Status: "ACTIVE", MaxMicrocredits: maxAmount, ExpiresAt: expiresAt.UTC()}
	var modelID, protocolCode any
	var formulaVersionID any
	var pricingSnapshot, requestSnapshot any
	if pricing != nil {
		modelID, protocolCode, formulaVersionID = pricing.ModelID, pricing.ProtocolCode, pricing.FormulaVersionID
		pricingSnapshot, requestSnapshot = []byte(pricing.PricingSnapshot), []byte(pricing.RequestSnapshot)
	}
	_, err = tx.Exec(ctx, `INSERT INTO usage_holds(id, usage_request_id, wallet_id, price_version_id, status, max_microcredits, expires_at, model_id, protocol_code, formula_version_id, pricing_snapshot, request_snapshot, created_at, updated_at) VALUES ($1, $2, $3, $4, 'ACTIVE', $5, $6, $7, $8, $9, $10, $11, $12, $12)`, hold.ID, usageRequestID, walletID, priceVersionID, maxAmount, hold.ExpiresAt, modelID, protocolCode, formulaVersionID, pricingSnapshot, requestSnapshot, now)
	if err != nil {
		return Hold{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO usage_hold_lease_events(id,hold_id,event_type,expires_at,previous_max_microcredits,max_microcredits,additional_microcredits,created_at) VALUES($1,$2,'CREATED',$3,0,$4,$4,$5)`, uuid.Must(uuid.NewV7()), hold.ID, hold.ExpiresAt, hold.MaxMicrocredits, now); err != nil {
		return Hold{}, err
	}
	for _, allocation := range allocations {
		command, err := tx.Exec(ctx, `UPDATE entitlement_components SET available_microcredits=available_microcredits-$2,reserved_microcredits=reserved_microcredits+$2,updated_at=$3 WHERE id=$1 AND available_microcredits>=$2`, allocation.componentID, allocation.amount, now)
		if err != nil {
			return Hold{}, err
		}
		if command.RowsAffected() != 1 {
			return Hold{}, ErrConflict
		}
		if _, err := tx.Exec(ctx, `INSERT INTO usage_hold_allocations(hold_id,package_id,component_id,reserved_microcredits,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$5)`, hold.ID, allocation.packageID, allocation.componentID, allocation.amount, now); err != nil {
			return Hold{}, err
		}
	}
	return hold, tx.Commit(ctx)
}

func (r *PostgresRepository) Release(ctx context.Context, usageRequestID, holdID uuid.UUID, reason string) (Hold, error) {
	return r.releaseHold(ctx, usageRequestID, holdID, reason, false)
}

func (r *PostgresRepository) releaseHold(ctx context.Context, usageRequestID, holdID uuid.UUID, reason string, expiryOnly bool) (Hold, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Hold{}, err
	}
	defer tx.Rollback(ctx)
	hold, err := getHoldByUsage(ctx, tx, usageRequestID, false)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && hold.ID != holdID) {
		return Hold{}, ErrNotFound
	}
	if err != nil {
		return Hold{}, err
	}
	if _, err := getWallet(ctx, tx, hold.WalletID, true); err != nil {
		return Hold{}, err
	}
	hold, err = getHoldByUsage(ctx, tx, usageRequestID, true)
	if err != nil || hold.ID != holdID {
		return Hold{}, ErrNotFound
	}
	if expiryOnly && hold.Status != "ACTIVE" {
		return Hold{}, ErrUsageAlreadyTerminal
	}
	if hold.Status == "RELEASED" {
		return hold, tx.Commit(ctx)
	}
	if hold.Status != "ACTIVE" {
		return Hold{}, ErrUsageAlreadyTerminal
	}
	now := r.now()
	if expiryOnly && hold.ExpiresAt.After(now) {
		return hold, tx.Commit(ctx)
	}
	snapshot, _ := json.Marshal(map[string]string{"release_reason": strings.TrimSpace(reason)})
	if err := releasePackageAllocations(ctx, tx, hold.ID, now); err != nil {
		return Hold{}, err
	}
	command, err := tx.Exec(ctx, `UPDATE wallets SET reserved_microcredits = reserved_microcredits - $2, version = version + 1, updated_at = $3 WHERE id = $1 AND reserved_microcredits >= $2`, hold.WalletID, hold.MaxMicrocredits, now)
	if err == nil && command.RowsAffected() != 1 {
		err = ErrConflict
	}
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE usage_holds SET status = 'RELEASED', usage_snapshot = $2, terminal_at = $3, updated_at = $3 WHERE id = $1`, hold.ID, snapshot, now)
	}
	if err != nil {
		return Hold{}, err
	}
	hold.Status = "RELEASED"
	return hold, tx.Commit(ctx)
}
