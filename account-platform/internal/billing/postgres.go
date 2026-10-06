package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	paymentClearingAccount = uuid.MustParse("00000000-0000-7000-8000-000000000001")
	usageRevenueAccount    = uuid.MustParse("00000000-0000-7000-8000-000000000002")
	refundClearingAccount  = uuid.MustParse("00000000-0000-7000-8000-000000000003")
	adjustmentAccount      = uuid.MustParse("00000000-0000-7000-8000-000000000004")
)

type PostgresRepository struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func NewPostgresRepository(ctx context.Context, databaseURL string) (*PostgresRepository, error) {
	configuration, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse billing database URL: %w", err)
	}
	configuration.MaxConns = 30
	configuration.MinConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		return nil, fmt.Errorf("open billing database: %w", err)
	}
	repository := &PostgresRepository{pool: pool, now: func() time.Time { return time.Now().UTC() }}
	if err := repository.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return repository, nil
}

func (r *PostgresRepository) Ping(ctx context.Context) error { return r.pool.Ping(ctx) }
func (r *PostgresRepository) Close()                         { r.pool.Close() }

func (r *PostgresRepository) EnsureWallet(ctx context.Context, ownerType string, ownerID uuid.UUID) (Wallet, bool, error) {
	if ownerType != "PERSONAL" && ownerType != "ORGANIZATION" && ownerType != "ORG_UNIT" {
		return Wallet{}, false, ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Wallet{}, false, err
	}
	defer tx.Rollback(ctx)
	if ownerType == "ORGANIZATION" {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "organization-lifecycle:"+ownerID.String()); err != nil {
			return Wallet{}, false, err
		}
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, ownerType+":"+ownerID.String()); err != nil {
		return Wallet{}, false, err
	}
	wallet, err := getWalletByOwner(ctx, tx, ownerType, ownerID)
	if err == nil {
		return wallet, false, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Wallet{}, false, err
	}
	now := r.now()
	walletStatus := "ACTIVE"
	if ownerType == "ORGANIZATION" {
		if scanErr := tx.QueryRow(ctx, `SELECT CASE status WHEN 'ACTIVE' THEN 'ACTIVE' WHEN 'DELETED' THEN 'CLOSED' ELSE 'FROZEN' END FROM organization_lifecycle_states WHERE organization_id=$1`, ownerID).Scan(&walletStatus); scanErr != nil && !errors.Is(scanErr, pgx.ErrNoRows) {
			return Wallet{}, false, scanErr
		}
	}
	wallet = Wallet{ID: uuid.Must(uuid.NewV7()), OwnerType: ownerType, OwnerID: ownerID, Status: walletStatus, Version: 1}
	_, err = tx.Exec(ctx, `INSERT INTO wallets(id, owner_type, owner_id, status, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $5)`, wallet.ID, ownerType, ownerID, walletStatus, now)
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO ledger_accounts(id, wallet_id, code, account_type, created_at) VALUES ($1, $2, $3, 'WALLET_LIABILITY', $4)`, uuid.Must(uuid.NewV7()), wallet.ID, "wallet:"+wallet.ID.String(), now)
	}
	if err != nil {
		return Wallet{}, false, err
	}
	return wallet, true, tx.Commit(ctx)
}

func (r *PostgresRepository) GetWallet(ctx context.Context, walletID uuid.UUID) (Wallet, error) {
	wallet, err := getWallet(ctx, r.pool, walletID, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return Wallet{}, ErrNotFound
	}
	return wallet, err
}

func (r *PostgresRepository) ListProducts(ctx context.Context, currency, channel string) ([]Product, error) {
	currency = strings.ToUpper(strings.TrimSpace(currency))
	channel = strings.ToUpper(strings.TrimSpace(channel))
	rows, err := r.pool.Query(ctx, `
SELECT product_id, plan_name, sku_code, amount_fen, credit_microcredits, display_points,
       base_points, bonus_points, validity_days, price_status, invoice_product_name,
       currency, channels, catalog_version
FROM recharge_products
WHERE enabled AND currency = $1 AND ($2 = '' OR $2 = ANY(channels))
ORDER BY amount_fen`, currency, channel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	products := make([]Product, 0)
	for rows.Next() {
		var product Product
		if err := rows.Scan(&product.ProductID, &product.PlanName, &product.SKUCode, &product.AmountFen, &product.CreditMicrocredits, &product.DisplayPoints, &product.BasePoints, &product.BonusPoints, &product.ValidityDays, &product.PriceStatus, &product.InvoiceProductName, &product.Currency, &product.Channels, &product.CatalogVersion); err != nil {
			return nil, err
		}
		products = append(products, product)
	}
	return products, rows.Err()
}

func (r *PostgresRepository) ActiveExternalProducts(ctx context.Context) (map[string]ExternalProduct, error) {
	rows, err := r.pool.Query(ctx, `SELECT product_id,sku_code,amount_fen FROM recharge_products WHERE enabled AND sku_code<>'' ORDER BY product_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	products := make(map[string]ExternalProduct)
	for rows.Next() {
		var productID string
		var product ExternalProduct
		if err := rows.Scan(&productID, &product.SKU, &product.AmountFen); err != nil {
			return nil, err
		}
		products[productID] = product
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(products) == 0 {
		return nil, errors.New("no enabled Mall products are configured")
	}
	return products, nil
}

func (r *PostgresRepository) CurrentPrice(ctx context.Context, model string) (PriceVersion, error) {
	var price PriceVersion
	err := r.pool.QueryRow(ctx, `SELECT id,model,input_per_million,output_per_million,cached_input_per_million,effective_from FROM pricing_versions WHERE model=$1 AND effective_from <= $2 AND (effective_to IS NULL OR effective_to > $2) ORDER BY effective_from DESC LIMIT 1`, strings.TrimSpace(model), r.now()).Scan(&price.ID, &price.Model, &price.InputPerMillion, &price.OutputPerMillion, &price.CachedInputPerMillion, &price.EffectiveFrom)
	if errors.Is(err, pgx.ErrNoRows) {
		return PriceVersion{}, ErrNotFound
	}
	return price, err
}

func (r *PostgresRepository) PriceByID(ctx context.Context, priceID uuid.UUID) (PriceVersion, error) {
	var price PriceVersion
	err := r.pool.QueryRow(ctx, `SELECT id,model,input_per_million,output_per_million,cached_input_per_million,effective_from FROM pricing_versions WHERE id=$1`, priceID).Scan(&price.ID, &price.Model, &price.InputPerMillion, &price.OutputPerMillion, &price.CachedInputPerMillion, &price.EffectiveFrom)
	if errors.Is(err, pgx.ErrNoRows) {
		return PriceVersion{}, ErrNotFound
	}
	return price, err
}

func (r *PostgresRepository) Credit(ctx context.Context, walletID uuid.UUID, businessReference string, amount int64, refundable bool) (Wallet, error) {
	if amount <= 0 || strings.TrimSpace(businessReference) == "" {
		return Wallet{}, ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Wallet{}, err
	}
	defer tx.Rollback(ctx)
	wallet, err := getWallet(ctx, tx, walletID, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return Wallet{}, ErrNotFound
	}
	if err != nil {
		return Wallet{}, err
	}
	if wallet.Status != "ACTIVE" {
		return Wallet{}, ErrWalletUnavailable
	}
	var existingAmount int64
	err = tx.QueryRow(ctx, `
SELECT COALESCE(sum(lp.delta_microcredits), 0)
FROM journal_entries je JOIN ledger_postings lp ON lp.journal_entry_id = je.id
JOIN ledger_accounts la ON la.id = lp.ledger_account_id
WHERE je.business_reference = $1 AND la.wallet_id = $2`, businessReference, walletID).Scan(&existingAmount)
	if err != nil {
		return Wallet{}, err
	}
	if existingAmount != 0 {
		if existingAmount != amount {
			return Wallet{}, ErrConflict
		}
		return wallet, tx.Commit(ctx)
	}
	entryID, lotID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	now := r.now()
	var expiresAt *time.Time
	if err := insertJournal(ctx, tx, entryID, "RECHARGE", businessReference, "credit wallet", now); err != nil {
		return Wallet{}, err
	}
	walletAccount, err := walletLedgerAccount(ctx, tx, walletID)
	if err != nil {
		return Wallet{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO credit_lots(id, wallet_id, source_type, source_reference, original_microcredits, remaining_microcredits, refundable, expires_at, created_at) VALUES ($1, $2, 'RECHARGE', $3, $4, $4, $5, $6, $7)`, lotID, walletID, businessReference, amount, refundable, expiresAt, now)
	packageID := uuid.Must(uuid.NewV7())
	if err == nil {
		var command pgconn.CommandTag
		command, err = tx.Exec(ctx, `INSERT INTO entitlement_packages(id,user_id,wallet_id,payment_order_id,credit_lot_id,product_id,plan_name,sku_code,amount_fen,points_total,points_remaining,credited_at,valid_from,valid_until,status,created_at,updated_at,source_type,source_reference) SELECT $1,CASE WHEN owner_type='PERSONAL' THEN owner_id ELSE NULL END,id,NULL,$2,'SYSTEM_CREDIT','System credit','',0,$3,$3,$4,$4,$5,'ACTIVE',$4,$4,'SYSTEM',$6 FROM wallets WHERE id=$7`, packageID, lotID, amount, now, expiresAt, businessReference, walletID)
		if err == nil && command.RowsAffected() != 1 {
			err = ErrConflict
		}
	}
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO entitlement_components(id,package_id,component_type,credit_lot_id,original_microcredits,available_microcredits,credited_at,expires_at,status,created_at,updated_at) VALUES($1,$2,'BASE',$1,$3,$3,$4,$5,'ACTIVE',$4,$4)`, lotID, packageID, amount, now, expiresAt)
	}
	if err == nil {
		err = insertPosting(ctx, tx, entryID, walletAccount, amount, &lotID, now)
	}
	if err == nil {
		err = insertPosting(ctx, tx, entryID, paymentClearingAccount, -amount, nil, now)
	}
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE wallets SET posted_microcredits = posted_microcredits + $2, version = version + 1, updated_at = $3 WHERE id = $1`, walletID, amount, now)
	}
	if err != nil {
		return Wallet{}, err
	}
	wallet.PostedMicrocredits += amount
	wallet.AvailableMicrocredits += amount
	wallet.Version++
	return wallet, tx.Commit(ctx)
}

func (r *PostgresRepository) Reserve(ctx context.Context, usageRequestID, walletID uuid.UUID, maxAmount int64, priceVersionID *uuid.UUID, expiresAt time.Time) (Hold, error) {
	if maxAmount <= 0 || !expiresAt.After(r.now()) {
		return Hold{}, ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Hold{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "usage.reserve:"+usageRequestID.String()); err != nil {
		return Hold{}, err
	}
	existing, err := getHoldByUsage(ctx, tx, usageRequestID, true)
	if err == nil {
		if existing.WalletID != walletID || existing.MaxMicrocredits != maxAmount || !equalUUID(existing.PriceVersionID, priceVersionID) {
			return Hold{}, ErrConflict
		}
		var memberBacked bool
		if err := tx.QueryRow(ctx, `SELECT quota_organization_id IS NOT NULL FROM usage_holds WHERE id=$1`, existing.ID).Scan(&memberBacked); err != nil {
			return Hold{}, err
		}
		if memberBacked {
			return Hold{}, ErrConflict
		}
		return existing, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Hold{}, err
	}
	now := r.now()
	wallet, err := getWallet(ctx, tx, walletID, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return Hold{}, ErrNotFound
	}
	if err != nil {
		return Hold{}, err
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
	rows, err := tx.Query(ctx, `SELECT component.package_id,component.id,GREATEST(component.available_microcredits-COALESCE((SELECT sum(refund.credit_microcredits) FROM refunds refund JOIN credit_lots lot ON lot.source_reference='recharge:'||refund.payment_order_id::text WHERE lot.id=component.credit_lot_id AND refund.status IN ('REQUESTED','MANUAL_REVIEW')),0),0)::bigint FROM entitlement_components component JOIN entitlement_packages package ON package.id=component.package_id WHERE package.wallet_id=$1 AND package.status='ACTIVE' AND package.valid_from<=$2 AND (component.expires_at IS NULL OR component.expires_at>$2) AND component.status='ACTIVE' AND component.available_microcredits>0 ORDER BY component.expires_at NULLS LAST,CASE component.component_type WHEN 'BONUS' THEN 0 ELSE 1 END,component.credited_at,component.id FOR UPDATE OF component`, walletID, now)
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
	if err := rows.Err(); err != nil {
		rows.Close()
		return Hold{}, err
	}
	rows.Close()
	if remaining != 0 {
		return Hold{}, ErrInsufficientBalance
	}
	command, err := tx.Exec(ctx, `
UPDATE wallets SET reserved_microcredits = reserved_microcredits + $2, version = version + 1, updated_at = $3
WHERE id = $1 AND status = 'ACTIVE' AND posted_microcredits - reserved_microcredits >= $2`, walletID, maxAmount, now)
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
	_, err = tx.Exec(ctx, `INSERT INTO usage_holds(id, usage_request_id, wallet_id, price_version_id, status, max_microcredits, expires_at, created_at, updated_at) VALUES ($1, $2, $3, $4, 'ACTIVE', $5, $6, $7, $7)`, hold.ID, usageRequestID, walletID, priceVersionID, maxAmount, hold.ExpiresAt, now)
	if err != nil {
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

func (r *PostgresRepository) Commit(ctx context.Context, usageRequestID, holdID uuid.UUID, actual int64, usage map[string]any) (Hold, error) {
	if actual < 0 {
		return Hold{}, ErrConflict
	}
	usageJSON, err := json.Marshal(usage)
	if err != nil {
		return Hold{}, ErrConflict
	}
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
	if hold.Status == "COMMITTED" && hold.ActualMicrocredits != nil && *hold.ActualMicrocredits == actual {
		return hold, tx.Commit(ctx)
	}
	if hold.Status == "RELEASED" && actual > 0 {
		var releaseReason string
		if err := tx.QueryRow(ctx, `SELECT COALESCE(usage_snapshot->>'release_reason','') FROM usage_holds WHERE id=$1`, hold.ID).Scan(&releaseReason); err != nil {
			return Hold{}, err
		}
		if releaseReason == "ENTITLEMENT_EXPIRED_UNCERTAIN" {
			now := r.now()
			_, err := tx.Exec(ctx, `INSERT INTO usage_loss_reviews(id,usage_request_id,hold_id,wallet_id,reason_code,claimed_actual_microcredits,usage_snapshot,status,created_at,updated_at) VALUES($1,$2,$3,$4,'ENTITLEMENT_EXPIRED_UNCERTAIN',$5,$6,'REVIEW_REQUIRED',$7,$7) ON CONFLICT(hold_id) DO NOTHING`, uuid.Must(uuid.NewV7()), usageRequestID, hold.ID, hold.WalletID, actual, usageJSON, now)
			if err != nil {
				return Hold{}, err
			}
			var sameClaim bool
			if err := tx.QueryRow(ctx, `SELECT usage_request_id=$2 AND wallet_id=$3 AND reason_code='ENTITLEMENT_EXPIRED_UNCERTAIN' AND claimed_actual_microcredits=$4 AND usage_snapshot=$5::jsonb FROM usage_loss_reviews WHERE hold_id=$1`, hold.ID, usageRequestID, hold.WalletID, actual, usageJSON).Scan(&sameClaim); err != nil {
				return Hold{}, err
			}
			if !sameClaim {
				return Hold{}, ErrConflict
			}
			if err := tx.Commit(ctx); err != nil {
				return Hold{}, err
			}
			return hold, ErrUsagePlatformLossReview
		}
	}
	if hold.Status != "ACTIVE" && hold.Status != "REVIEW" {
		return Hold{}, ErrUsageAlreadyTerminal
	}
	if actual > hold.MaxMicrocredits {
		return Hold{}, ErrConflict
	}
	now := r.now()
	entryID := uuid.Nil
	var walletAccount uuid.UUID
	if actual > 0 {
		entryID = uuid.Must(uuid.NewV7())
		if err := insertJournal(ctx, tx, entryID, "USAGE", "usage:"+usageRequestID.String(), "settle model usage", now); err != nil {
			return Hold{}, err
		}
		walletAccount, err = walletLedgerAccount(ctx, tx, hold.WalletID)
		if err != nil {
			return Hold{}, err
		}
	}
	type allocation struct {
		packageID   uuid.UUID
		componentID uuid.UUID
		lotID       uuid.UUID
		reserved    int64
	}
	rows, err := tx.Query(ctx, `SELECT allocation.package_id,allocation.component_id,component.credit_lot_id,allocation.reserved_microcredits FROM usage_hold_allocations allocation JOIN entitlement_components component ON component.id=allocation.component_id WHERE allocation.hold_id=$1 ORDER BY component.expires_at NULLS LAST,CASE component.component_type WHEN 'BONUS' THEN 0 ELSE 1 END,component.credited_at,component.id FOR UPDATE OF allocation,component`, hold.ID)
	if err != nil {
		return Hold{}, err
	}
	allocations := make([]allocation, 0)
	for rows.Next() {
		var item allocation
		if err := rows.Scan(&item.packageID, &item.componentID, &item.lotID, &item.reserved); err != nil {
			rows.Close()
			return Hold{}, err
		}
		allocations = append(allocations, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Hold{}, err
	}
	rows.Close()
	remaining := actual
	for _, item := range allocations {
		settled := min(remaining, item.reserved)
		released := item.reserved - settled
		if settled > 0 {
			command, err := tx.Exec(ctx, `UPDATE credit_lots SET remaining_microcredits=remaining_microcredits-$2 WHERE id=$1 AND remaining_microcredits>=$2`, item.lotID, settled)
			if err != nil {
				return Hold{}, err
			}
			if command.RowsAffected() != 1 {
				return Hold{}, ErrConflict
			}
			if err := insertPosting(ctx, tx, entryID, walletAccount, -settled, &item.lotID, now); err != nil {
				return Hold{}, err
			}
		}
		memberBacked, memberErr := hasMemberQuotaAllocations(ctx, tx, hold.ID, item.componentID)
		if memberErr != nil {
			return Hold{}, memberErr
		}
		if memberBacked {
			if err := settleMemberQuotaComponent(ctx, tx, hold.ID, item.componentID, settled, now); err != nil {
				return Hold{}, err
			}
		} else {
			command, err := tx.Exec(ctx, `UPDATE entitlement_components SET reserved_microcredits=reserved_microcredits-$2,used_microcredits=used_microcredits+$3,available_microcredits=available_microcredits+$4,status=CASE WHEN reserved_microcredits-$2=0 AND available_microcredits+$4+active_grant_microcredits+suspended_grant_microcredits=0 THEN 'CONSUMED' ELSE 'ACTIVE' END,updated_at=$5 WHERE id=$1 AND reserved_microcredits>=$2`, item.componentID, item.reserved, settled, released, now)
			if err != nil {
				return Hold{}, err
			}
			if command.RowsAffected() != 1 {
				return Hold{}, ErrConflict
			}
		}
		command, err := tx.Exec(ctx, `UPDATE usage_hold_allocations SET settled_microcredits=$2,released_microcredits=$3,updated_at=$4 WHERE hold_id=$1 AND component_id=$5`, hold.ID, settled, released, now, item.componentID)
		if err != nil {
			return Hold{}, err
		}
		if command.RowsAffected() != 1 {
			return Hold{}, ErrConflict
		}
		remaining -= settled
	}
	if _, err := tx.Exec(ctx, `UPDATE entitlement_packages package SET status=CASE WHEN EXISTS(SELECT 1 FROM entitlement_components component WHERE component.package_id=package.id AND (component.available_microcredits+component.reserved_microcredits+component.active_grant_microcredits+component.suspended_grant_microcredits)>0) THEN 'ACTIVE' ELSE 'CONSUMED' END,updated_at=$2 WHERE package.id IN (SELECT package_id FROM usage_hold_allocations WHERE hold_id=$1)`, hold.ID, now); err != nil {
		return Hold{}, err
	}
	if remaining != 0 {
		return Hold{}, ErrConflict
	}
	if actual > 0 {
		if err := insertPosting(ctx, tx, entryID, usageRevenueAccount, actual, nil, now); err != nil {
			return Hold{}, err
		}
	}
	command, err := tx.Exec(ctx, `UPDATE wallets SET posted_microcredits = posted_microcredits - $2, reserved_microcredits = reserved_microcredits - $3, version = version + 1, updated_at = $4 WHERE id = $1 AND reserved_microcredits >= $3 AND posted_microcredits >= $2`, hold.WalletID, actual, hold.MaxMicrocredits, now)
	if err == nil && command.RowsAffected() != 1 {
		return Hold{}, ErrConflict
	}
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE usage_holds SET status = 'COMMITTED', actual_microcredits = $2, usage_snapshot = $3, terminal_at = $4, updated_at = $4 WHERE id = $1`, hold.ID, actual, usageJSON, now)
	}
	if err != nil {
		return Hold{}, err
	}
	hold.Status = "COMMITTED"
	hold.ActualMicrocredits = &actual
	return hold, tx.Commit(ctx)
}

func (r *PostgresRepository) Release(ctx context.Context, usageRequestID, holdID uuid.UUID, reason string) (Hold, error) {
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
	if hold.Status == "RELEASED" {
		return hold, tx.Commit(ctx)
	}
	if hold.Status != "ACTIVE" && hold.Status != "REVIEW" {
		return Hold{}, ErrUsageAlreadyTerminal
	}
	now := r.now()
	snapshot, _ := json.Marshal(map[string]string{"release_reason": strings.TrimSpace(reason)})
	if _, err := getWallet(ctx, tx, hold.WalletID, true); err != nil {
		return Hold{}, err
	}
	if err := releasePackageAllocations(ctx, tx, hold.ID, now); err != nil {
		return Hold{}, err
	}
	command, err := tx.Exec(ctx, `UPDATE wallets SET reserved_microcredits = reserved_microcredits - $2, version = version + 1, updated_at = $3 WHERE id = $1 AND reserved_microcredits >= $2`, hold.WalletID, hold.MaxMicrocredits, now)
	if err == nil && command.RowsAffected() != 1 {
		return Hold{}, ErrConflict
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

func releasePackageAllocations(ctx context.Context, tx pgx.Tx, holdID uuid.UUID, now time.Time) error {
	var memberCount int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM member_quota_hold_allocations WHERE hold_id=$1 AND settled_microcredits=0 AND released_microcredits=0`, holdID).Scan(&memberCount); err != nil {
		return err
	}
	if memberCount > 0 {
		rows, err := tx.Query(ctx, `SELECT quota.id,quota.organization_id,quota.member_user_id,quota.package_id,quota.component_id,quota.component_type,quota.original_microcredits,quota.available_microcredits,quota.reserved_microcredits,quota.used_microcredits,quota.expired_microcredits,quota.revoked_microcredits,quota.status,quota.suspension_reason,quota.membership_version,quota.allocation_version,quota.created_by,quota.expires_at,quota.created_at,quota.updated_at,allocation.reserved_microcredits
FROM member_quota_hold_allocations allocation JOIN member_quota_grants quota ON quota.id=allocation.grant_id
WHERE allocation.hold_id=$1 AND allocation.settled_microcredits=0 AND allocation.released_microcredits=0 ORDER BY quota.component_id,quota.id FOR UPDATE OF allocation,quota`, holdID)
		if err != nil {
			return err
		}
		items := make([]memberHoldAllocation, 0, memberCount)
		for rows.Next() {
			var item memberHoldAllocation
			if err := scanMemberQuotaGrantWithAmount(rows, &item.grant, &item.amount); err != nil {
				rows.Close()
				return err
			}
			items = append(items, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, item := range items {
			bucket := "active_grant_microcredits"
			if item.grant.Status == "SUSPENDED" {
				bucket = "suspended_grant_microcredits"
			}
			query := `UPDATE entitlement_components SET reserved_microcredits=reserved_microcredits-$2,` + bucket + `=` + bucket + `+$2,updated_at=$3 WHERE id=$1 AND reserved_microcredits>=$2`
			command, err := tx.Exec(ctx, query, item.grant.ComponentID, item.amount, now)
			if err != nil || command.RowsAffected() != 1 {
				if err == nil {
					err = ErrConflict
				}
				return err
			}
			command, err = tx.Exec(ctx, `UPDATE member_quota_grants SET reserved_microcredits=reserved_microcredits-$2,available_microcredits=available_microcredits+$2,allocation_version=allocation_version+1,updated_at=$3 WHERE id=$1 AND reserved_microcredits>=$2`, item.grant.ID, item.amount, now)
			if err != nil || command.RowsAffected() != 1 {
				if err == nil {
					err = ErrConflict
				}
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE member_quota_hold_allocations SET released_microcredits=reserved_microcredits,updated_at=$3 WHERE hold_id=$1 AND grant_id=$2`, holdID, item.grant.ID, now); err != nil {
				return err
			}
			item.grant.ReservedMicrocredits -= item.amount
			item.grant.AvailableMicrocredits += item.amount
			item.grant.AllocationVersion++
			if err := appendMemberQuotaAudit(ctx, tx, item.grant, "HOLD_RELEASED", nil, item.amount, now); err != nil {
				return err
			}
		}
		command, err := tx.Exec(ctx, `UPDATE usage_hold_allocations SET released_microcredits=reserved_microcredits,updated_at=$2 WHERE hold_id=$1 AND settled_microcredits=0 AND released_microcredits=0`, holdID, now)
		if err != nil {
			return err
		}
		var expected int64
		if err := tx.QueryRow(ctx, `SELECT count(DISTINCT component_id) FROM member_quota_hold_allocations WHERE hold_id=$1`, holdID).Scan(&expected); err != nil {
			return err
		}
		if command.RowsAffected() != expected {
			return ErrConflict
		}
		return nil
	}
	var expected int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM usage_hold_allocations WHERE hold_id=$1 AND settled_microcredits=0 AND released_microcredits=0`, holdID).Scan(&expected); err != nil {
		return err
	}
	if expected == 0 {
		return ErrConflict
	}
	command, err := tx.Exec(ctx, `UPDATE entitlement_components component SET reserved_microcredits=component.reserved_microcredits-allocation.reserved_microcredits,available_microcredits=component.available_microcredits+allocation.reserved_microcredits,status='ACTIVE',updated_at=$2 FROM usage_hold_allocations allocation WHERE allocation.hold_id=$1 AND allocation.component_id=component.id AND allocation.settled_microcredits=0 AND allocation.released_microcredits=0 AND component.reserved_microcredits>=allocation.reserved_microcredits`, holdID, now)
	if err != nil {
		return err
	}
	if command.RowsAffected() != expected {
		return ErrConflict
	}
	command, err = tx.Exec(ctx, `UPDATE usage_hold_allocations SET released_microcredits=reserved_microcredits,updated_at=$2 WHERE hold_id=$1 AND settled_microcredits=0 AND released_microcredits=0`, holdID, now)
	if err != nil {
		return err
	}
	if command.RowsAffected() != expected {
		return ErrConflict
	}
	return nil
}

func (r *PostgresRepository) ExpireHolds(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 1000 {
		limit = 100
	}
	// Expiry is not evidence that the provider produced no usage (ADR-005).
	command, err := r.pool.Exec(ctx, `WITH expired AS (
SELECT id FROM usage_holds WHERE status='ACTIVE' AND expires_at <= $1
ORDER BY expires_at LIMIT $2 FOR UPDATE SKIP LOCKED
)
UPDATE usage_holds h SET status='REVIEW',updated_at=$1 FROM expired e
WHERE h.id=e.id AND h.status='ACTIVE'`, r.now(), limit)
	return int(command.RowsAffected()), err
}

// The wallet is locked before this query. Pending or uncertain refunds retain
// their original recharge lot, rather than only a fungible wallet reservation.
const availableLotsQuery = `SELECT lot.id,GREATEST(component.available_microcredits-COALESCE((SELECT sum(refund.credit_microcredits) FROM refunds refund WHERE refund.status IN ('REQUESTED','MANUAL_REVIEW') AND lot.source_reference='recharge:'||refund.payment_order_id::text),0),0)::bigint
FROM credit_lots lot JOIN entitlement_components component ON component.credit_lot_id=lot.id JOIN entitlement_packages package ON package.id=component.package_id
WHERE lot.wallet_id=$1 AND component.available_microcredits>0 AND component.status='ACTIVE' AND package.status='ACTIVE' AND package.valid_from<=$2
AND (component.expires_at IS NULL OR component.expires_at>$2)
ORDER BY component.expires_at NULLS LAST,CASE component.component_type WHEN 'BONUS' THEN 0 ELSE 1 END,component.credited_at,component.id FOR UPDATE OF lot,component`

// Caller holds the wallet lock. Every generic debit updates the lot and its
// component in the same transaction; package totals are projected by the trigger.
func consumeAvailableLot(ctx context.Context, tx pgx.Tx, lotID uuid.UUID, amount int64, now time.Time) error {
	if amount <= 0 {
		return ErrConflict
	}
	command, err := tx.Exec(ctx, `UPDATE credit_lots SET remaining_microcredits=remaining_microcredits-$2 WHERE id=$1 AND remaining_microcredits>=$2`, lotID, amount)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	command, err = tx.Exec(ctx, `UPDATE entitlement_components SET available_microcredits=available_microcredits-$2,used_microcredits=used_microcredits+$2,status=CASE WHEN available_microcredits=$2 AND reserved_microcredits+active_grant_microcredits+suspended_grant_microcredits=0 THEN 'CONSUMED' ELSE status END,updated_at=$3 WHERE credit_lot_id=$1 AND status='ACTIVE' AND available_microcredits>=$2`, lotID, amount, now)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	_, err = tx.Exec(ctx, `UPDATE entitlement_packages SET status=CASE WHEN points_remaining=0 AND points_reserved=0 THEN 'CONSUMED' ELSE status END,updated_at=$2 WHERE credit_lot_id=$1`, lotID, now)
	return err
}

type queryable interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// lockTransactionKey serializes the first insert for an idempotency scope.
// SELECT ... FOR UPDATE cannot lock a row that does not exist yet, so without
// this transaction-scoped lock concurrent first requests can all pass the
// lookup and then race on a unique constraint.
func lockTransactionKey(ctx context.Context, tx pgx.Tx, scope, key string) error {
	lockKey := fmt.Sprintf("%d:%s%s", len(scope), scope, key)
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockKey)
	return err
}

func getWallet(ctx context.Context, database queryable, walletID uuid.UUID, lock bool) (Wallet, error) {
	query := `SELECT id, owner_type, owner_id, status, posted_microcredits, reserved_microcredits, posted_microcredits - reserved_microcredits, version FROM wallets WHERE id = $1`
	if lock {
		query += ` FOR UPDATE`
	}
	var wallet Wallet
	err := database.QueryRow(ctx, query, walletID).Scan(&wallet.ID, &wallet.OwnerType, &wallet.OwnerID, &wallet.Status, &wallet.PostedMicrocredits, &wallet.ReservedMicrocredits, &wallet.AvailableMicrocredits, &wallet.Version)
	return wallet, err
}

func getWalletByOwner(ctx context.Context, database queryable, ownerType string, ownerID uuid.UUID) (Wallet, error) {
	var wallet Wallet
	err := database.QueryRow(ctx, `SELECT id, owner_type, owner_id, status, posted_microcredits, reserved_microcredits, posted_microcredits - reserved_microcredits, version FROM wallets WHERE owner_type = $1 AND owner_id = $2`, ownerType, ownerID).Scan(&wallet.ID, &wallet.OwnerType, &wallet.OwnerID, &wallet.Status, &wallet.PostedMicrocredits, &wallet.ReservedMicrocredits, &wallet.AvailableMicrocredits, &wallet.Version)
	return wallet, err
}

func getHoldByUsage(ctx context.Context, database queryable, usageRequestID uuid.UUID, lock bool) (Hold, error) {
	query := `SELECT id, usage_request_id, wallet_id, price_version_id, status, max_microcredits, actual_microcredits, expires_at FROM usage_holds WHERE usage_request_id = $1`
	if lock {
		query += ` FOR UPDATE`
	}
	var hold Hold
	err := database.QueryRow(ctx, query, usageRequestID).Scan(&hold.ID, &hold.UsageRequestID, &hold.WalletID, &hold.PriceVersionID, &hold.Status, &hold.MaxMicrocredits, &hold.ActualMicrocredits, &hold.ExpiresAt)
	return hold, err
}

func walletLedgerAccount(ctx context.Context, tx pgx.Tx, walletID uuid.UUID) (uuid.UUID, error) {
	var accountID uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM ledger_accounts WHERE wallet_id = $1`, walletID).Scan(&accountID)
	return accountID, err
}

func insertJournal(ctx context.Context, tx pgx.Tx, id uuid.UUID, entryType, reference, description string, createdAt time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO journal_entries(id, entry_type, business_reference, description, created_at) VALUES ($1, $2, $3, $4, $5)`, id, entryType, reference, description, createdAt)
	return err
}

func insertPosting(ctx context.Context, tx pgx.Tx, journalID, accountID uuid.UUID, delta int64, lotID *uuid.UUID, createdAt time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO ledger_postings(id, journal_entry_id, ledger_account_id, delta_microcredits, credit_lot_id, created_at) VALUES ($1, $2, $3, $4, $5, $6)`, uuid.Must(uuid.NewV7()), journalID, accountID, delta, lotID, createdAt)
	return err
}

func equalUUID(left, right *uuid.UUID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
