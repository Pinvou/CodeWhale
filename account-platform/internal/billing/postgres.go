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
	pool                      *pgxpool.Pool
	now                       func() time.Time
	paginationMetrics         *AdminPaginationMetrics
	paginationMaxStorageBytes int64
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
	repository := &PostgresRepository{pool: pool, now: func() time.Time { return time.Now().UTC() }, paginationMetrics: newAdminPaginationMetrics(), paginationMaxStorageBytes: adminSnapshotMaxStorageBytes}
	if err := repository.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if err := repository.ValidateSystemLedgerAccounts(ctx); err != nil {
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
SELECT product.product_id, plan_name, sku_code, amount_fen, credit_microcredits, display_points,
       base_points, bonus_points, validity_days, price_status, invoice_product_name,
       currency, channels, catalog_version
FROM recharge_products product
JOIN recharge_product_catalog_items item ON item.product_id=product.product_id
WHERE item.lifecycle_status='ON_SALE' AND product.currency = $1 AND ($2 = '' OR $2 = ANY(product.channels))
ORDER BY item.display_order,product.product_id`, currency, channel)
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
		product.AvailabilityStatus = "AVAILABLE"
		products = append(products, product)
	}
	return products, rows.Err()
}

func (r *PostgresRepository) ApplyProductAvailability(ctx context.Context, providerID, providerVersion string, products []Product) ([]Product, error) {
	var providerEnabled bool
	if err := r.pool.QueryRow(ctx, `SELECT COALESCE((SELECT new_orders_enabled FROM payment_provider_controls WHERE provider_id=$1 AND provider_version=$2),TRUE)`, providerID, providerVersion).Scan(&providerEnabled); err != nil {
		return nil, err
	}
	pausedProducts := map[string]bool{}
	rows, err := r.pool.Query(ctx, `SELECT product_id FROM payment_product_controls WHERE provider_id=$1 AND provider_version=$2 AND NOT new_orders_enabled`, providerID, providerVersion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var productID string
		if err := rows.Scan(&productID); err != nil {
			return nil, err
		}
		pausedProducts[productID] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for index := range products {
		products[index].AvailabilityStatus = "AVAILABLE"
		products[index].AvailabilityReasonCode = ""
		if !providerEnabled {
			products[index].AvailabilityStatus = "PROVIDER_PAUSED"
			products[index].AvailabilityReasonCode = "PAYMENT_PROVIDER_PAUSED"
		} else if pausedProducts[products[index].ProductID] {
			products[index].AvailabilityStatus = "PRODUCT_PAUSED"
			products[index].AvailabilityReasonCode = "PAYMENT_PRODUCT_PAUSED"
		}
	}
	return products, nil
}

func (r *PostgresRepository) ActiveExternalProducts(ctx context.Context, providerID, providerVersion string, allowTestOnly bool) (map[string]ExternalProduct, error) {
	rows, err := r.pool.Query(ctx, `SELECT mapping.id,mapping.product_id,mapping.mapping_version,mapping.provider_id,mapping.provider_version,mapping.external_sku,mapping.amount_fen,mapping.currency,mapping.channels,mapping.status
FROM payment_product_mappings mapping
JOIN recharge_product_catalog_items item ON item.product_id=mapping.product_id AND item.lifecycle_status='ON_SALE'
JOIN product_admission_bindings binding ON binding.catalog_item_id=item.catalog_item_id AND binding.mapping_id=mapping.id AND binding.effective_from<=now() AND (binding.effective_to IS NULL OR binding.effective_to>now())
JOIN product_admission_evidence evidence ON evidence.evidence_id=binding.evidence_id AND evidence.expires_at>now()
WHERE mapping.provider_id=$1 AND mapping.provider_version=$2 AND mapping.effective_from<=now() AND (mapping.effective_to IS NULL OR mapping.effective_to>now()) AND (mapping.status='FROZEN' OR ($3 AND mapping.status='TEST_ONLY'))
AND NOT EXISTS(SELECT 1 FROM product_admission_revocations revocation WHERE revocation.evidence_id=evidence.evidence_id)
ORDER BY mapping.product_id,mapping.mapping_version DESC`, providerID, providerVersion, allowTestOnly)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	products := make(map[string]ExternalProduct)
	for rows.Next() {
		var productID string
		var product ExternalProduct
		if err := rows.Scan(&product.MappingID, &productID, &product.MappingVersion, &product.ProviderID, &product.ProviderVersion, &product.SKU, &product.AmountFen, &product.Currency, &product.Channels, &product.Status); err != nil {
			return nil, err
		}
		if _, duplicate := products[productID]; duplicate {
			return nil, fmt.Errorf("multiple active payment product mappings for %s", productID)
		}
		products[productID] = product
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return products, nil
}

func (r *PostgresRepository) ExternalProductBySKU(ctx context.Context, providerID, providerVersion, sku string) (ExternalProduct, error) {
	var product ExternalProduct
	err := r.pool.QueryRow(ctx, `SELECT id,mapping_version,provider_id,provider_version,external_sku,amount_fen,currency,channels,status
FROM payment_product_mappings
WHERE provider_id=$1 AND provider_version=$2 AND external_sku=$3
ORDER BY mapping_version DESC LIMIT 1`, providerID, providerVersion, strings.TrimSpace(sku)).Scan(
		&product.MappingID, &product.MappingVersion, &product.ProviderID, &product.ProviderVersion,
		&product.SKU, &product.AmountFen, &product.Currency, &product.Channels, &product.Status)
	return product, err
}

func (r *PostgresRepository) CurrentPrice(ctx context.Context, model string) (PriceVersion, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return PriceVersion{}, ErrConflict
	}
	now := r.now()
	var formulaID uuid.UUID
	err := r.pool.QueryRow(ctx, `SELECT binding.formula_version_id FROM billing_protocol_formula_bindings binding WHERE binding.protocol_code='CHAT' AND EXISTS(SELECT 1 FROM pricing_versions price WHERE price.protocol_code=binding.protocol_code AND price.formula_version_id=binding.formula_version_id AND price.credit_unit='MICROCREDIT' AND price.lifecycle_status IN ('PUBLISHED','ACTIVE') AND price.published_at IS NOT NULL AND price.cancelled_at IS NULL AND price.effective_from<=$2 AND (price.effective_to IS NULL OR price.effective_to>$2) AND (price.scope_type='GLOBAL' OR price.scope_type='MODEL' AND price.scope_id=$1)) ORDER BY binding.published_at DESC,binding.formula_version_id DESC LIMIT 1`, model, now).Scan(&formulaID)
	if errors.Is(err, pgx.ErrNoRows) {
		return PriceVersion{}, ErrPriceMissing
	}
	if err != nil {
		return PriceVersion{}, err
	}
	return r.ResolveCurrentPrice(ctx, model, "CHAT", formulaID, "MICROCREDIT", now)
}

func (r *PostgresRepository) PriceByID(ctx context.Context, priceID uuid.UUID) (PriceVersion, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return PriceVersion{}, err
	}
	defer tx.Rollback(ctx)
	var candidate priceCandidate
	err = tx.QueryRow(ctx, `SELECT id,scope_type,COALESCE(scope_id,''),protocol_code,formula_version_id,credit_unit,effective_from,effective_to FROM pricing_versions WHERE id=$1 AND published_at IS NOT NULL`, priceID).Scan(&candidate.id, &candidate.scopeType, &candidate.model, &candidate.protocol, &candidate.formulaID, &candidate.creditUnit, &candidate.effectiveFrom, &candidate.effectiveTo)
	if errors.Is(err, pgx.ErrNoRows) {
		return PriceVersion{}, ErrNotFound
	}
	if err != nil {
		return PriceVersion{}, err
	}
	model := candidate.model
	if candidate.scopeType == "GLOBAL" {
		model = "*"
	}
	price, err := resolvePriceCandidate(ctx, tx, candidate, model, candidate.effectiveFrom)
	if err != nil {
		return PriceVersion{}, err
	}
	return price, tx.Commit(ctx)
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
	if wallet.Status != "ACTIVE" || wallet.FinancialStatus != "CLEAR" {
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
	expiresAt := now.AddDate(0, 0, 30)
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
			_, err := tx.Exec(ctx, `INSERT INTO usage_loss_reviews(id,usage_request_id,hold_id,wallet_id,reason_code,claimed_actual_microcredits,usage_snapshot,status,created_at,updated_at) VALUES($1,$2,$3,$4,'ENTITLEMENT_EXPIRED_UNCERTAIN',$5,$6,'REVIEW_REQUIRED',$7,$7) ON CONFLICT(hold_id) DO UPDATE SET claimed_actual_microcredits=EXCLUDED.claimed_actual_microcredits,usage_snapshot=EXCLUDED.usage_snapshot,updated_at=EXCLUDED.updated_at`, uuid.Must(uuid.NewV7()), usageRequestID, hold.ID, hold.WalletID, actual, usageJSON, now)
			if err != nil {
				return Hold{}, err
			}
			if err := tx.Commit(ctx); err != nil {
				return Hold{}, err
			}
			return hold, ErrUsagePlatformLossReview
		}
	}
	if hold.Status != "ACTIVE" {
		return Hold{}, ErrUsageAlreadyTerminal
	}
	now := r.now()
	backedActual := min(actual, hold.MaxMicrocredits)
	overage := actual - backedActual
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
	rows, err := tx.Query(ctx, `SELECT allocation.package_id,allocation.component_id,component.credit_lot_id,allocation.reserved_microcredits FROM usage_hold_allocations allocation JOIN entitlement_components component ON component.id=allocation.component_id WHERE allocation.hold_id=$1 ORDER BY component.expires_at,CASE component.component_type WHEN 'BONUS' THEN 0 ELSE 1 END,component.credited_at,component.id FOR UPDATE OF allocation,component`, hold.ID)
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
	rows.Close()
	remaining := backedActual
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
			command, err := tx.Exec(ctx, `UPDATE entitlement_components SET reserved_microcredits=reserved_microcredits-$2,used_microcredits=used_microcredits+$3,available_microcredits=available_microcredits+$4,status=CASE WHEN reserved_microcredits-$2=0 AND available_microcredits+$4=0 THEN 'CONSUMED' ELSE 'ACTIVE' END,updated_at=$5 WHERE id=$1 AND reserved_microcredits>=$2`, item.componentID, item.reserved, settled, released, now)
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
	if _, err := tx.Exec(ctx, `UPDATE entitlement_packages package SET status=CASE WHEN EXISTS(SELECT 1 FROM entitlement_components component WHERE component.package_id=package.id AND (component.available_microcredits>0 OR component.reserved_microcredits>0)) THEN 'ACTIVE' ELSE 'CONSUMED' END,updated_at=$2 WHERE package.id IN (SELECT package_id FROM usage_hold_allocations WHERE hold_id=$1)`, hold.ID, now); err != nil {
		return Hold{}, err
	}
	if remaining != 0 {
		return Hold{}, errors.New("usage hold is not backed by entitlement packages")
	}
	if overage > 0 {
		if err := insertPosting(ctx, tx, entryID, receivableAccount, -overage, nil, now); err != nil {
			return Hold{}, err
		}
		if _, err := createReceivableWithinTransaction(ctx, tx, hold.WalletID, "USAGE_OVERAGE", usageRequestID.String(), overage, nil, nil, nil, &usageRequestID, now); err != nil {
			return Hold{}, err
		}
	}
	if actual > 0 {
		if err := insertPosting(ctx, tx, entryID, usageRevenueAccount, actual, nil, now); err != nil {
			return Hold{}, err
		}
	}
	command, err := tx.Exec(ctx, `UPDATE wallets SET posted_microcredits = posted_microcredits - $2, reserved_microcredits = reserved_microcredits - $3, version = version + 1, updated_at = $4 WHERE id = $1 AND reserved_microcredits >= $3 AND posted_microcredits >= $2`, hold.WalletID, backedActual, hold.MaxMicrocredits, now)
	if err == nil && command.RowsAffected() != 1 {
		err = ErrConflict
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

func (r *PostgresRepository) ExtendHold(ctx context.Context, usageRequestID, holdID uuid.UUID, expiresAt time.Time, additional int64) (Hold, error) {
	if usageRequestID == uuid.Nil || holdID == uuid.Nil || additional < 0 || !expiresAt.After(r.now()) {
		return Hold{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Hold{}, err
	}
	defer tx.Rollback(ctx)
	hold, err := getHoldByUsage(ctx, tx, usageRequestID, true)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && hold.ID != holdID) {
		return Hold{}, ErrNotFound
	}
	if err != nil {
		return Hold{}, err
	}
	if hold.Status != "ACTIVE" {
		return Hold{}, ErrUsageAlreadyTerminal
	}
	now := r.now()
	previousExpiresAt, previousMaximum := hold.ExpiresAt, hold.MaxMicrocredits
	if additional > 0 {
		var organizationID, memberUserID *uuid.UUID
		var membershipVersion *int64
		if err := tx.QueryRow(ctx, `SELECT organization_id,member_user_id,membership_version FROM usage_holds WHERE id=$1`, hold.ID).Scan(&organizationID, &memberUserID, &membershipVersion); err != nil {
			return Hold{}, err
		}
		if organizationID != nil && memberUserID != nil && membershipVersion != nil {
			if err := r.extendMemberQuotaHold(ctx, tx, hold, *organizationID, *memberUserID, *membershipVersion, additional, now); err != nil {
				return Hold{}, err
			}
		} else if err := extendPersonalHold(ctx, tx, hold, additional, now); err != nil {
			return Hold{}, err
		}
		hold.MaxMicrocredits += additional
	}
	if expiresAt.After(hold.ExpiresAt) {
		hold.ExpiresAt = expiresAt.UTC()
	}
	if _, err := tx.Exec(ctx, `UPDATE usage_holds SET expires_at=$2,max_microcredits=$3,updated_at=$4 WHERE id=$1 AND status='ACTIVE'`, hold.ID, hold.ExpiresAt, hold.MaxMicrocredits, now); err != nil {
		return Hold{}, err
	}
	if hold.ExpiresAt.After(previousExpiresAt) || hold.MaxMicrocredits != previousMaximum {
		if _, err := tx.Exec(ctx, `INSERT INTO usage_hold_lease_events(id,hold_id,event_type,previous_expires_at,expires_at,previous_max_microcredits,max_microcredits,additional_microcredits,created_at) VALUES($1,$2,'EXTENDED',$3,$4,$5,$6,$7,$8)`, uuid.Must(uuid.NewV7()), hold.ID, previousExpiresAt, hold.ExpiresAt, previousMaximum, hold.MaxMicrocredits, hold.MaxMicrocredits-previousMaximum, now); err != nil {
			return Hold{}, err
		}
	}
	return hold, tx.Commit(ctx)
}

func extendPersonalHold(ctx context.Context, tx pgx.Tx, hold Hold, additional int64, now time.Time) error {
	wallet, err := getWallet(ctx, tx, hold.WalletID, true)
	if err != nil {
		return err
	}
	if wallet.Status != "ACTIVE" || wallet.AvailableMicrocredits < additional {
		return ErrInsufficientBalance
	}
	type allocation struct {
		packageID, componentID uuid.UUID
		amount                 int64
	}
	rows, err := tx.Query(ctx, `SELECT component.package_id,component.id,component.available_microcredits FROM entitlement_components component JOIN entitlement_packages package ON package.id=component.package_id WHERE package.wallet_id=$1 AND package.status='ACTIVE' AND package.valid_from<=$2 AND component.expires_at>$2 AND component.status='ACTIVE' AND component.available_microcredits>0 ORDER BY component.expires_at,CASE component.component_type WHEN 'BONUS' THEN 0 ELSE 1 END,component.credited_at,component.id FOR UPDATE OF component`, hold.WalletID, now)
	if err != nil {
		return err
	}
	remaining := additional
	items := make([]allocation, 0)
	for rows.Next() {
		var item allocation
		var available int64
		if err := rows.Scan(&item.packageID, &item.componentID, &available); err != nil {
			rows.Close()
			return err
		}
		item.amount = min(remaining, available)
		if item.amount > 0 {
			items = append(items, item)
			remaining -= item.amount
		}
		if remaining == 0 {
			break
		}
	}
	rows.Close()
	if remaining != 0 {
		return ErrInsufficientBalance
	}
	command, err := tx.Exec(ctx, `UPDATE wallets SET reserved_microcredits=reserved_microcredits+$2,version=version+1,updated_at=$3 WHERE id=$1 AND status='ACTIVE' AND receivable_microcredits=0 AND posted_microcredits-reserved_microcredits>=$2`, hold.WalletID, additional, now)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrInsufficientBalance
	}
	for _, item := range items {
		command, err = tx.Exec(ctx, `UPDATE entitlement_components SET available_microcredits=available_microcredits-$2,reserved_microcredits=reserved_microcredits+$2,updated_at=$3 WHERE id=$1 AND available_microcredits>=$2`, item.componentID, item.amount, now)
		if err != nil || command.RowsAffected() != 1 {
			if err != nil {
				return err
			}
			return ErrConflict
		}
		_, err = tx.Exec(ctx, `INSERT INTO usage_hold_allocations(hold_id,package_id,component_id,reserved_microcredits,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$5) ON CONFLICT(hold_id,component_id) DO UPDATE SET reserved_microcredits=usage_hold_allocations.reserved_microcredits+EXCLUDED.reserved_microcredits,updated_at=EXCLUDED.updated_at`, hold.ID, item.packageID, item.componentID, item.amount, now)
		if err != nil {
			return err
		}
	}
	return nil
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
	if hold.Status != "ACTIVE" {
		return Hold{}, ErrUsageAlreadyTerminal
	}
	now := r.now()
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

func releasePackageAllocations(ctx context.Context, tx pgx.Tx, holdID uuid.UUID, now time.Time) error {
	var memberCount int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM member_quota_hold_allocations WHERE hold_id=$1 AND settled_microcredits=0 AND released_microcredits=0`, holdID).Scan(&memberCount); err != nil {
		return err
	}
	if memberCount > 0 {
		rows, err := tx.Query(ctx, `SELECT grant.id,grant.organization_id,grant.member_user_id,grant.package_id,grant.component_id,grant.component_type,grant.original_microcredits,grant.available_microcredits,grant.reserved_microcredits,grant.used_microcredits,grant.expired_microcredits,grant.revoked_microcredits,grant.status,grant.suspension_reason,grant.membership_version,grant.allocation_version,grant.created_by,grant.expires_at,grant.created_at,grant.updated_at,allocation.reserved_microcredits
FROM member_quota_hold_allocations allocation JOIN member_quota_grants grant ON grant.id=allocation.grant_id
WHERE allocation.hold_id=$1 AND allocation.settled_microcredits=0 AND allocation.released_microcredits=0 ORDER BY grant.component_id,grant.id FOR UPDATE OF allocation,grant`, holdID)
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
	rows, err := r.pool.Query(ctx, `SELECT usage_request_id, id FROM usage_holds WHERE status = 'ACTIVE' AND expires_at <= $1 ORDER BY expires_at LIMIT $2`, r.now(), limit)
	if err != nil {
		return 0, err
	}
	type candidate struct{ usageID, holdID uuid.UUID }
	candidates := make([]candidate, 0)
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.usageID, &item.holdID); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, item)
	}
	rows.Close()
	count := 0
	for _, candidate := range candidates {
		if _, err := r.Release(ctx, candidate.usageID, candidate.holdID, "hold lease expired"); err == nil {
			count++
		} else if !errors.Is(err, ErrUsageAlreadyTerminal) {
			return count, err
		}
	}
	return count, nil
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
	query := `SELECT id,owner_type,owner_id,status,posted_microcredits,reserved_microcredits,receivable_microcredits,posted_microcredits-reserved_microcredits-receivable_microcredits,financial_status,financial_freeze_reason,version FROM wallets WHERE id=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	var wallet Wallet
	err := database.QueryRow(ctx, query, walletID).Scan(&wallet.ID, &wallet.OwnerType, &wallet.OwnerID, &wallet.Status, &wallet.PostedMicrocredits, &wallet.ReservedMicrocredits, &wallet.ReceivableMicrocredits, &wallet.AvailableMicrocredits, &wallet.FinancialStatus, &wallet.FinancialFreezeReason, &wallet.Version)
	return wallet, err
}

func getWalletByOwner(ctx context.Context, database queryable, ownerType string, ownerID uuid.UUID) (Wallet, error) {
	var wallet Wallet
	err := database.QueryRow(ctx, `SELECT id,owner_type,owner_id,status,posted_microcredits,reserved_microcredits,receivable_microcredits,posted_microcredits-reserved_microcredits-receivable_microcredits,financial_status,financial_freeze_reason,version FROM wallets WHERE owner_type=$1 AND owner_id=$2`, ownerType, ownerID).Scan(&wallet.ID, &wallet.OwnerType, &wallet.OwnerID, &wallet.Status, &wallet.PostedMicrocredits, &wallet.ReservedMicrocredits, &wallet.ReceivableMicrocredits, &wallet.AvailableMicrocredits, &wallet.FinancialStatus, &wallet.FinancialFreezeReason, &wallet.Version)
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
