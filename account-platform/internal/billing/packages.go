package billing

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type PackageSummary struct {
	AvailableMicrocredits      string     `json:"available_microcredits"`
	BaseAvailableMicrocredits  string     `json:"base_available_microcredits"`
	BonusAvailableMicrocredits string     `json:"bonus_available_microcredits"`
	ReservedMicrocredits       string     `json:"reserved_microcredits"`
	UsedMicrocredits           string     `json:"used_microcredits"`
	ActivePackageCount         int        `json:"active_package_count"`
	NextExpiryAt               *time.Time `json:"next_expiry_at,omitempty"`
	NextExpiryMicrocredits     string     `json:"next_expiry_microcredits"`
}

func (r *PostgresRepository) ListPackages(ctx context.Context, userID, walletID uuid.UUID, limit int) ([]EntitlementPackage, PackageSummary, error) {
	if userID == uuid.Nil || walletID == uuid.Nil {
		return nil, PackageSummary{}, ErrConflict
	}
	return r.listPackages(ctx, userID, walletID, limit, false)
}

func (r *PostgresRepository) ListOrganizationPackages(ctx context.Context, organizationID, walletID uuid.UUID, limit int) ([]EntitlementPackage, PackageSummary, error) {
	wallet, err := r.GetWallet(ctx, walletID)
	if err != nil {
		return nil, PackageSummary{}, err
	}
	if organizationID == uuid.Nil || wallet.OwnerType != "ORGANIZATION" || wallet.OwnerID != organizationID {
		return nil, PackageSummary{}, ErrConflict
	}
	return r.listPackages(ctx, uuid.Nil, walletID, limit, true)
}

func (r *PostgresRepository) listPackages(ctx context.Context, userID, walletID uuid.UUID, limit int, organization bool) ([]EntitlementPackage, PackageSummary, error) {
	if limit < 1 || limit > 200 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx, `SELECT ep.id,ep.user_id,ep.wallet_id,ep.payment_order_id,ep.source_payment_transaction_id,COALESCE(po.merchant_order_no,''),ep.source_type,ep.source_reference,ep.product_id,ep.plan_name,ep.sku_code,ep.amount_fen,ep.points_total,ep.points_reserved,ep.points_used,ep.points_remaining,ep.points_expired,ep.points_revoked,ep.credited_at,ep.valid_from,ep.valid_until,ep.status FROM entitlement_packages ep LEFT JOIN payment_orders po ON po.id=ep.payment_order_id WHERE ep.wallet_id=$2 AND ($4 OR (NOT $4 AND ep.user_id=$1 AND ep.valid_until IS NOT NULL AND ep.payment_order_id IS NOT NULL)) ORDER BY ep.credited_at DESC,ep.id DESC LIMIT $3`, userID, walletID, limit, organization)
	if err != nil {
		return nil, PackageSummary{}, err
	}
	defer rows.Close()
	items := make([]EntitlementPackage, 0)
	for rows.Next() {
		var item EntitlementPackage
		if err := rows.Scan(&item.ID, &item.UserID, &item.WalletID, &item.PaymentOrderID, &item.PaymentTransactionID, &item.ProviderOrderReference, &item.SourceType, &item.SourceReference, &item.ProductID, &item.PlanName, &item.SKUCode, &item.AmountFen, &item.PointsTotal, &item.PointsReserved, &item.PointsUsed, &item.PointsRemaining, &item.PointsExpired, &item.PointsRevoked, &item.CreditedAt, &item.ValidFrom, &item.ValidUntil, &item.Status); err != nil {
			return nil, PackageSummary{}, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, PackageSummary{}, err
	}
	if len(items) > 0 {
		packageIDs := make([]uuid.UUID, 0, len(items))
		index := make(map[uuid.UUID]int, len(items))
		for i := range items {
			packageIDs = append(packageIDs, items[i].ID)
			index[items[i].ID] = i
			items[i].Components = []EntitlementComponent{}
		}
		componentRows, err := r.pool.Query(ctx, `SELECT package_id,id,component_type,original_microcredits,available_microcredits,reserved_microcredits,used_microcredits,active_grant_microcredits,suspended_grant_microcredits,expired_microcredits,revoked_microcredits,credited_at,expires_at,status FROM entitlement_components WHERE package_id=ANY($1) ORDER BY expires_at,CASE component_type WHEN 'BONUS' THEN 0 ELSE 1 END,credited_at,id`, packageIDs)
		if err != nil {
			return nil, PackageSummary{}, err
		}
		defer componentRows.Close()
		for componentRows.Next() {
			var packageID uuid.UUID
			var component EntitlementComponent
			if err := componentRows.Scan(&packageID, &component.ID, &component.ComponentType, &component.OriginalMicrocredits, &component.AvailableMicrocredits, &component.ReservedMicrocredits, &component.UsedMicrocredits, &component.ActiveGrantMicrocredits, &component.SuspendedGrantMicrocredits, &component.ExpiredMicrocredits, &component.RevokedMicrocredits, &component.CreditedAt, &component.ExpiresAt, &component.Status); err != nil {
				return nil, PackageSummary{}, err
			}
			items[index[packageID]].Components = append(items[index[packageID]].Components, component)
		}
		if err := componentRows.Err(); err != nil {
			return nil, PackageSummary{}, err
		}
	}
	summary := PackageSummary{AvailableMicrocredits: "0", BaseAvailableMicrocredits: "0", BonusAvailableMicrocredits: "0", ReservedMicrocredits: "0", UsedMicrocredits: "0", NextExpiryMicrocredits: "0"}
	var nextExpiry *time.Time
	err = r.pool.QueryRow(ctx, `SELECT COALESCE(sum(component.available_microcredits),0),COALESCE(sum(component.available_microcredits) FILTER(WHERE component.component_type='BASE'),0),COALESCE(sum(component.available_microcredits) FILTER(WHERE component.component_type='BONUS'),0),COALESCE(sum(component.reserved_microcredits),0),COALESCE(sum(component.used_microcredits),0),count(DISTINCT package.id),min(component.expires_at) FILTER(WHERE component.available_microcredits>0) FROM entitlement_components component JOIN entitlement_packages package ON package.id=component.package_id WHERE package.wallet_id=$2 AND ($4 OR package.user_id=$1 OR package.user_id IS NULL) AND component.status='ACTIVE' AND (component.expires_at>$3 OR ($4 AND component.expires_at IS NULL))`, userID, walletID, r.now(), organization).Scan(&summary.AvailableMicrocredits, &summary.BaseAvailableMicrocredits, &summary.BonusAvailableMicrocredits, &summary.ReservedMicrocredits, &summary.UsedMicrocredits, &summary.ActivePackageCount, &nextExpiry)
	if err != nil {
		return nil, PackageSummary{}, err
	}
	summary.NextExpiryAt = nextExpiry
	if nextExpiry != nil {
		if err := r.pool.QueryRow(ctx, `SELECT COALESCE(sum(component.available_microcredits),0) FROM entitlement_components component JOIN entitlement_packages package ON package.id=component.package_id WHERE package.wallet_id=$2 AND ($4 OR package.user_id=$1 OR package.user_id IS NULL) AND component.status='ACTIVE' AND component.expires_at=$3`, userID, walletID, *nextExpiry, organization).Scan(&summary.NextExpiryMicrocredits); err != nil {
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
	var packageID, walletID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id,wallet_id FROM entitlement_packages WHERE status='ACTIVE' AND valid_until<=$1 AND points_reserved=0 ORDER BY valid_until,credited_at,id LIMIT 1`, now).Scan(&packageID, &walletID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, err
	}
	if err := lockTransactionKey(ctx, tx, "entitlement.expire", packageID.String()); err != nil {
		return false, err
	}
	if _, err := getWallet(ctx, tx, walletID, true); err != nil {
		return false, err
	}
	var stillDue bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM entitlement_packages WHERE id=$1 AND status='ACTIVE' AND valid_until<=$2 AND points_reserved=0)`, packageID, now).Scan(&stillDue); err != nil {
		return false, err
	}
	if !stillDue {
		return true, tx.Commit(ctx)
	}
	type expiringComponent struct {
		id, lotID                    uuid.UUID
		available, active, suspended int64
	}
	componentRows, err := tx.Query(ctx, `SELECT id,credit_lot_id,available_microcredits,active_grant_microcredits,suspended_grant_microcredits FROM entitlement_components WHERE package_id=$1 ORDER BY expires_at,CASE component_type WHEN 'BONUS' THEN 0 ELSE 1 END,credited_at,id FOR UPDATE`, packageID)
	if err != nil {
		return false, err
	}
	components := make([]expiringComponent, 0)
	var remaining int64
	for componentRows.Next() {
		var component expiringComponent
		if err := componentRows.Scan(&component.id, &component.lotID, &component.available, &component.active, &component.suspended); err != nil {
			componentRows.Close()
			return false, err
		}
		components = append(components, component)
		remaining += component.available + component.active + component.suspended
	}
	if err := componentRows.Err(); err != nil {
		componentRows.Close()
		return false, err
	}
	componentRows.Close()
	if remaining > 0 {
		entryID := uuid.Must(uuid.NewV7())
		if err := insertJournal(ctx, tx, entryID, "EXPIRE", "expire:"+packageID.String(), "expire entitlement package", now); err != nil {
			return false, err
		}
		walletAccount, err := walletLedgerAccount(ctx, tx, walletID)
		if err != nil {
			return false, err
		}
		for _, component := range components {
			expiring := component.available + component.active + component.suspended
			if expiring == 0 {
				continue
			}
			if err := insertPosting(ctx, tx, entryID, walletAccount, -expiring, &component.lotID, now); err != nil {
				return false, err
			}
			command, err := tx.Exec(ctx, `UPDATE credit_lots SET remaining_microcredits=remaining_microcredits-$2 WHERE id=$1 AND remaining_microcredits>=$2`, component.lotID, expiring)
			if err != nil {
				return false, err
			}
			if command.RowsAffected() != 1 {
				return false, ErrConflict
			}
			if err := expireMemberQuotaGrantsForComponent(ctx, tx, component.id, now); err != nil {
				return false, err
			}
			command, err = tx.Exec(ctx, `UPDATE entitlement_components SET available_microcredits=0,active_grant_microcredits=0,suspended_grant_microcredits=0,expired_microcredits=expired_microcredits+$2,status='EXPIRED',updated_at=$6 WHERE id=$1 AND available_microcredits=$3 AND active_grant_microcredits=$4 AND suspended_grant_microcredits=$5 AND reserved_microcredits=0`, component.id, expiring, component.available, component.active, component.suspended, now)
			if err != nil {
				return false, err
			}
			if command.RowsAffected() != 1 {
				return false, ErrConflict
			}
		}
		if err := insertPosting(ctx, tx, entryID, adjustmentAccount, remaining, nil, now); err != nil {
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
	status := "EXPIRED"
	if remaining == 0 {
		status = "CONSUMED"
	}
	if _, err := tx.Exec(ctx, `UPDATE entitlement_packages SET status=$2,updated_at=$3 WHERE id=$1`, packageID, status, now); err != nil {
		return false, err
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
