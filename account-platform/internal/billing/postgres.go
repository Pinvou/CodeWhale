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
	wallet = Wallet{ID: uuid.Must(uuid.NewV7()), OwnerType: ownerType, OwnerID: ownerID, Status: "ACTIVE", Version: 1}
	_, err = tx.Exec(ctx, `INSERT INTO wallets(id, owner_type, owner_id, status, created_at, updated_at) VALUES ($1, $2, $3, 'ACTIVE', $4, $4)`, wallet.ID, ownerType, ownerID, now)
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
SELECT product_id, amount_fen, credit_microcredits, display_points, currency, channels, catalog_version
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
		if err := rows.Scan(&product.ProductID, &product.AmountFen, &product.CreditMicrocredits, &product.DisplayPoints, &product.Currency, &product.Channels, &product.CatalogVersion); err != nil {
			return nil, err
		}
		products = append(products, product)
	}
	return products, rows.Err()
}

func (r *PostgresRepository) CurrentPrice(ctx context.Context, model string) (PriceVersion, error) {
	var price PriceVersion
	err := r.pool.QueryRow(ctx, `SELECT id,model,input_per_million,output_per_million,cached_input_per_million,effective_from FROM pricing_versions WHERE model=$1 AND effective_from <= $2 AND (effective_to IS NULL OR effective_to > $2) ORDER BY effective_from DESC LIMIT 1`, strings.TrimSpace(model), r.now()).Scan(&price.ID, &price.Model, &price.InputPerMillion, &price.OutputPerMillion, &price.CachedInputPerMillion, &price.EffectiveFrom)
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
	if err := insertJournal(ctx, tx, entryID, "RECHARGE", businessReference, "credit wallet", now); err != nil {
		return Wallet{}, err
	}
	walletAccount, err := walletLedgerAccount(ctx, tx, walletID)
	if err != nil {
		return Wallet{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO credit_lots(id, wallet_id, source_type, source_reference, original_microcredits, remaining_microcredits, refundable, created_at) VALUES ($1, $2, 'RECHARGE', $3, $4, $4, $5, $6)`, lotID, walletID, businessReference, amount, refundable, now)
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
		return existing, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Hold{}, err
	}
	command, err := tx.Exec(ctx, `
UPDATE wallets SET reserved_microcredits = reserved_microcredits + $2, version = version + 1, updated_at = $3
WHERE id = $1 AND status = 'ACTIVE' AND posted_microcredits - reserved_microcredits >= $2`, walletID, maxAmount, r.now())
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
	now := r.now()
	hold := Hold{ID: uuid.Must(uuid.NewV7()), UsageRequestID: usageRequestID, WalletID: walletID, PriceVersionID: priceVersionID, Status: "ACTIVE", MaxMicrocredits: maxAmount, ExpiresAt: expiresAt.UTC()}
	_, err = tx.Exec(ctx, `INSERT INTO usage_holds(id, usage_request_id, wallet_id, price_version_id, status, max_microcredits, expires_at, created_at, updated_at) VALUES ($1, $2, $3, $4, 'ACTIVE', $5, $6, $7, $7)`, hold.ID, usageRequestID, walletID, priceVersionID, maxAmount, hold.ExpiresAt, now)
	if err != nil {
		return Hold{}, err
	}
	return hold, tx.Commit(ctx)
}

func (r *PostgresRepository) Commit(ctx context.Context, usageRequestID, holdID uuid.UUID, actual int64, usage map[string]any) (Hold, error) {
	if actual < 0 {
		return Hold{}, ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
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
	if hold.Status == "COMMITTED" && hold.ActualMicrocredits != nil && *hold.ActualMicrocredits == actual {
		return hold, tx.Commit(ctx)
	}
	if hold.Status != "ACTIVE" && hold.Status != "REVIEW" {
		return Hold{}, ErrUsageAlreadyTerminal
	}
	if actual > hold.MaxMicrocredits {
		return Hold{}, ErrConflict
	}
	// Wallet precedes lots in every debit path, including refunds and adjustments.
	if _, err := getWallet(ctx, tx, hold.WalletID, true); err != nil {
		return Hold{}, err
	}
	usageJSON, err := json.Marshal(usage)
	if err != nil {
		return Hold{}, ErrConflict
	}
	now := r.now()
	if actual > 0 {
		entryID := uuid.Must(uuid.NewV7())
		if err := insertJournal(ctx, tx, entryID, "USAGE", "usage:"+usageRequestID.String(), "settle model usage", now); err != nil {
			return Hold{}, err
		}
		walletAccount, err := walletLedgerAccount(ctx, tx, hold.WalletID)
		if err != nil {
			return Hold{}, err
		}
		remaining := actual
		rows, err := tx.Query(ctx, availableLotsQuery, hold.WalletID, now)
		if err != nil {
			return Hold{}, err
		}
		type lot struct {
			id        uuid.UUID
			remaining int64
		}
		lots := make([]lot, 0)
		for rows.Next() {
			var item lot
			if err := rows.Scan(&item.id, &item.remaining); err != nil {
				rows.Close()
				return Hold{}, err
			}
			lots = append(lots, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return Hold{}, err
		}
		rows.Close()
		for _, item := range lots {
			if remaining == 0 {
				break
			}
			consume := min(remaining, item.remaining)
			if consume == 0 {
				continue
			}
			if _, err := tx.Exec(ctx, `UPDATE credit_lots SET remaining_microcredits = remaining_microcredits - $2 WHERE id = $1`, item.id, consume); err != nil {
				return Hold{}, err
			}
			if err := insertPosting(ctx, tx, entryID, walletAccount, -consume, &item.id, now); err != nil {
				return Hold{}, err
			}
			remaining -= consume
		}
		if remaining != 0 {
			return Hold{}, errors.New("wallet balance is not backed by credit lots")
		}
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
	hold, err := getHoldByUsage(ctx, tx, usageRequestID, true)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && hold.ID != holdID) {
		return Hold{}, ErrNotFound
	}
	if err != nil {
		return Hold{}, err
	}
	if hold.Status == "RELEASED" {
		return hold, tx.Commit(ctx)
	}
	if hold.Status != "ACTIVE" && hold.Status != "REVIEW" {
		return Hold{}, ErrUsageAlreadyTerminal
	}
	now := r.now()
	snapshot, _ := json.Marshal(map[string]string{"release_reason": strings.TrimSpace(reason)})
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
const availableLotsQuery = `SELECT l.id, GREATEST(l.remaining_microcredits - COALESCE((
SELECT sum(r.credit_microcredits) FROM refunds r
WHERE r.status IN ('REQUESTED','MANUAL_REVIEW')
AND l.source_reference='recharge:' || r.payment_order_id::text
),0),0)::bigint
FROM credit_lots l WHERE l.wallet_id=$1 AND l.remaining_microcredits > 0
AND (l.expires_at IS NULL OR l.expires_at > $2)
ORDER BY l.refundable DESC,l.created_at,l.id FOR UPDATE OF l`

type queryable interface {
	QueryRow(context.Context, string, ...any) pgx.Row
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
