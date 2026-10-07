package billing

import (
	"context"

	"errors"
	"fmt"

	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jackc/pgx/v5/pgxpool"
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

// NewPostgresRepositoryWithPool binds the Billing repository to a pool the
// caller owns. Production wiring uses NewPostgresRepository; this entry exists
// for cross-domain integration fixtures that must place the Billing runtime
// under the deployment session identity while another domain owns the database
// lifecycle. It grants no authority of its own.
func NewPostgresRepositoryWithPool(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{
		pool: pool, now: func() time.Time { return time.Now().UTC() },
	}
}

func (r *PostgresRepository) Ping(ctx context.Context) error { return r.pool.Ping(ctx) }

func (r *PostgresRepository) Close() { r.pool.Close() }

func (r *PostgresRepository) EnsureWallet(ctx context.Context, ownerType string, ownerID uuid.UUID) (Wallet, bool, error) {
	if ownerType != "PERSONAL" || ownerID == uuid.Nil {
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

type queryable interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func getWallet(ctx context.Context, database queryable, walletID uuid.UUID, lock bool) (Wallet, error) {
	query := `SELECT id,owner_type,owner_id,status,posted_microcredits,reserved_microcredits,posted_microcredits-reserved_microcredits,version FROM wallets WHERE id=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	var wallet Wallet
	err := database.QueryRow(ctx, query, walletID).Scan(&wallet.ID, &wallet.OwnerType, &wallet.OwnerID, &wallet.Status, &wallet.PostedMicrocredits, &wallet.ReservedMicrocredits, &wallet.AvailableMicrocredits, &wallet.Version)
	return wallet, err
}

func getWalletByOwner(ctx context.Context, database queryable, ownerType string, ownerID uuid.UUID) (Wallet, error) {
	var wallet Wallet
	err := database.QueryRow(ctx, `SELECT id,owner_type,owner_id,status,posted_microcredits,reserved_microcredits,posted_microcredits-reserved_microcredits,version FROM wallets WHERE owner_type=$1 AND owner_id=$2`, ownerType, ownerID).Scan(&wallet.ID, &wallet.OwnerType, &wallet.OwnerID, &wallet.Status, &wallet.PostedMicrocredits, &wallet.ReservedMicrocredits, &wallet.AvailableMicrocredits, &wallet.Version)
	return wallet, err
}
