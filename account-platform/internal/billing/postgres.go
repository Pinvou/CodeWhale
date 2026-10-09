package billing

import (
	"context"

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

// NewPostgresRepositoryWithPool binds the Billing repository to a pool the
// caller owns. Production wiring uses NewPostgresRepository; this entry exists
// for cross-domain integration fixtures that must place the Billing runtime
// under the deployment session identity while another domain owns the database
// lifecycle. It grants no authority of its own.

func NewPostgresRepositoryWithPool(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{
		pool: pool, now: func() time.Time { return time.Now().UTC() },
		paginationMetrics: newAdminPaginationMetrics(), paginationMaxStorageBytes: adminSnapshotMaxStorageBytes,
	}
}

func (r *PostgresRepository) Ping(ctx context.Context) error { return r.pool.Ping(ctx) }

func (r *PostgresRepository) Close() { r.pool.Close() }

func (r *PostgresRepository) EnsureWallet(ctx context.Context, ownerType string, ownerID uuid.UUID) (Wallet, bool, error) {
	if (ownerType != "PERSONAL" && ownerType != "ORGANIZATION" && ownerType != "ORG_UNIT") || ownerID == uuid.Nil {
		return Wallet{}, false, ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Wallet{}, false, err
	}
	defer tx.Rollback(ctx)
	wallet, created, err := r.ensureWalletWithinTransaction(ctx, tx, ownerType, ownerID)
	if err != nil {
		return Wallet{}, false, err
	}
	return wallet, created, tx.Commit(ctx)
}

func (r *PostgresRepository) ensureWalletWithinTransaction(ctx context.Context, tx pgx.Tx, ownerType string, ownerID uuid.UUID) (Wallet, bool, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, ownerType+":"+ownerID.String()); err != nil {
		return Wallet{}, false, err
	}
	wallet, err := getWalletByOwner(ctx, tx, ownerType, ownerID)
	if err == nil {
		return wallet, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Wallet{}, false, err
	}
	now := r.now()
	walletStatus := "ACTIVE"
	if ownerType == "ORGANIZATION" {
		if err := tx.QueryRow(ctx, `SELECT CASE status WHEN 'ACTIVE' THEN 'ACTIVE' WHEN 'DELETED' THEN 'CLOSED' ELSE 'FROZEN' END FROM organization_lifecycle_states WHERE organization_id=$1`, ownerID).Scan(&walletStatus); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return Wallet{}, false, err
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
	wallet, err = getWalletByOwner(ctx, tx, ownerType, ownerID)
	return wallet, err == nil, err
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
	command, err := tx.Exec(ctx, `UPDATE entitlement_components component SET reserved_microcredits=component.reserved_microcredits-allocation.reserved_microcredits,available_microcredits=component.available_microcredits+allocation.reserved_microcredits,status=CASE WHEN component.status IN ('FROZEN','REVOKED','EXPIRED') THEN component.status ELSE 'ACTIVE' END,updated_at=$2 FROM usage_hold_allocations allocation WHERE allocation.hold_id=$1 AND allocation.component_id=component.id AND allocation.settled_microcredits=0 AND allocation.released_microcredits=0 AND component.reserved_microcredits>=allocation.reserved_microcredits`, holdID, now)
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
