package billing

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type PackageSummary struct {
	AvailableMicrocredits int64     `json:"available_microcredits"`
	ActivePackageCount    int       `json:"active_package_count"`
	NextExpiryAt          *time.Time `json:"next_expiry_at,omitempty"`
	NextExpiryMicrocredits int64    `json:"next_expiry_microcredits"`
}

func (r *PostgresRepository) ListPackages(ctx context.Context, userID, walletID uuid.UUID, limit int) ([]EntitlementPackage, PackageSummary, error) {
	if userID == uuid.Nil || walletID == uuid.Nil {
		return nil, PackageSummary{}, ErrConflict
	}
	if limit < 1 || limit > 200 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx, `SELECT ep.id,ep.user_id,ep.wallet_id,ep.payment_order_id,po.merchant_order_no,ep.product_id,ep.plan_name,ep.sku_code,ep.amount_fen,ep.points_total,ep.points_reserved,ep.points_used,ep.points_remaining,ep.credited_at,ep.valid_from,ep.valid_until,ep.status FROM entitlement_packages ep JOIN payment_orders po ON po.id=ep.payment_order_id WHERE ep.user_id=$1 AND ep.wallet_id=$2 ORDER BY ep.credited_at DESC,ep.id DESC LIMIT $3`, userID, walletID, limit)
	if err != nil {
		return nil, PackageSummary{}, err
	}
	defer rows.Close()
	items := make([]EntitlementPackage, 0)
	for rows.Next() {
		var item EntitlementPackage
		if err := rows.Scan(&item.ID, &item.UserID, &item.WalletID, &item.PaymentOrderID, &item.MallOrderID, &item.ProductID, &item.PlanName, &item.SKUCode, &item.AmountFen, &item.PointsTotal, &item.PointsReserved, &item.PointsUsed, &item.PointsRemaining, &item.CreditedAt, &item.ValidFrom, &item.ValidUntil, &item.Status); err != nil {
			return nil, PackageSummary{}, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, PackageSummary{}, err
	}
	var summary PackageSummary
	var nextExpiry *time.Time
	err = r.pool.QueryRow(ctx, `SELECT COALESCE(sum(points_remaining),0),count(*),min(valid_until) FROM entitlement_packages WHERE user_id=$1 AND wallet_id=$2 AND status='ACTIVE' AND valid_until>$3 AND points_remaining>0`, userID, walletID, r.now()).Scan(&summary.AvailableMicrocredits, &summary.ActivePackageCount, &nextExpiry)
	if err != nil {
		return nil, PackageSummary{}, err
	}
	summary.NextExpiryAt = nextExpiry
	if nextExpiry != nil {
		if err := r.pool.QueryRow(ctx, `SELECT COALESCE(sum(points_remaining),0) FROM entitlement_packages WHERE user_id=$1 AND wallet_id=$2 AND status='ACTIVE' AND valid_until=$3`, userID, walletID, *nextExpiry).Scan(&summary.NextExpiryMicrocredits); err != nil {
			return nil, PackageSummary{}, err
		}
	}
	return items, summary, nil
}

func (r *PostgresRepository) ExpirePackages(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	count := 0
	for count < limit {
		expired, err := r.expireOnePackage(ctx)
		if err != nil {
			return count, err
		}
		if !expired {
			break
		}
		count++
	}
	return count, nil
}

func (r *PostgresRepository) expireOnePackage(ctx context.Context) (bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	now := r.now()
	var packageID, walletID, lotID uuid.UUID
	var remaining int64
	err = tx.QueryRow(ctx, `SELECT id,wallet_id,credit_lot_id,points_remaining FROM entitlement_packages WHERE status='ACTIVE' AND valid_until<=$1 AND points_reserved=0 ORDER BY valid_until,credited_at,id FOR UPDATE SKIP LOCKED LIMIT 1`, now).Scan(&packageID, &walletID, &lotID, &remaining)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, err
	}
	status := "EXPIRED"
	if remaining == 0 {
		status = "CONSUMED"
	}
	if _, err := tx.Exec(ctx, `UPDATE entitlement_packages SET status=$2,updated_at=$3 WHERE id=$1`, packageID, status, now); err != nil {
		return false, err
	}
	if remaining > 0 {
		entryID := uuid.Must(uuid.NewV7())
		if err := insertJournal(ctx, tx, entryID, "EXPIRE", "expire:"+packageID.String(), "expire entitlement package", now); err != nil {
			return false, err
		}
		walletAccount, err := walletLedgerAccount(ctx, tx, walletID)
		if err != nil {
			return false, err
		}
		if err := insertPosting(ctx, tx, entryID, walletAccount, -remaining, &lotID, now); err != nil {
			return false, err
		}
		if err := insertPosting(ctx, tx, entryID, adjustmentAccount, remaining, nil, now); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, `UPDATE credit_lots SET remaining_microcredits=0 WHERE id=$1`, lotID); err != nil {
			return false, err
		}
		command, err := tx.Exec(ctx, `UPDATE wallets SET posted_microcredits=posted_microcredits-$2,version=version+1,updated_at=$3 WHERE id=$1 AND posted_microcredits-reserved_microcredits>=$2`, walletID, remaining, now)
		if err != nil {
			return false, err
		}
		if command.RowsAffected() != 1 {
			return false, ErrConflict
		}
	}
	return true, tx.Commit(ctx)
}

type PackageExpiryWorker struct {
	repository *PostgresRepository
	interval   time.Duration
}

func NewPackageExpiryWorker(repository *PostgresRepository, interval time.Duration) *PackageExpiryWorker {
	if interval < time.Minute {
		interval = time.Minute
	}
	return &PackageExpiryWorker{repository: repository, interval: interval}
}

func (w *PackageExpiryWorker) Run(ctx context.Context) {
	_, _ = w.repository.ExpirePackages(ctx, 100)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = w.repository.ExpirePackages(ctx, 100)
		}
	}
}
