package billing

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type entitlementCorrectionReadPlan struct {
	PackageID            uuid.UUID
	WalletID             uuid.UUID
	PaymentOrderID       uuid.UUID
	PaymentTransactionID uuid.UUID
	SourceComponentID    uuid.UUID
	SourceLotID          uuid.UUID
	ExistingTargetID     uuid.UUID
	Amounts              entitlementCorrectionAmounts
}

// planEntitlementComponentCorrection is a read-only, unexposed precheck. It
// cannot grant execution: approval proof and the eventual Billing write must
// independently re-read every fact under their own deadlines and locks.
func (r *PostgresRepository) planEntitlementComponentCorrection(ctx context.Context, packageID, orderID uuid.UUID) (entitlementCorrectionReadPlan, error) {
	if r == nil || r.pool == nil || packageID == uuid.Nil || orderID == uuid.Nil {
		return entitlementCorrectionReadPlan{}, ErrConflict
	}
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	tx, err := r.pool.BeginTx(bounded, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly})
	if err != nil {
		return entitlementCorrectionReadPlan{}, err
	}
	defer tx.Rollback(bounded)
	var walletID, transactionID uuid.UUID
	var packageStatus, sourceType, walletStatus, orderStatus, fulfillmentStatus, transactionStatus string
	var packageTotal, remaining, reserved, used, expired, revoked, orderCredits, orderAmount, transactionAmount, walletVersion int64
	var packageUntil time.Time
	var orderCurrency, transactionCurrency, productID, sourceReference string
	var rawProduct []byte
	err = tx.QueryRow(bounded, `SELECT p.wallet_id,p.source_payment_transaction_id,p.status,p.source_type,
 p.points_total,p.points_remaining,p.points_reserved,p.points_used,p.points_expired,p.points_revoked,p.valid_until,
 w.status,w.version,o.status,o.fulfillment_status,o.credit_microcredits,o.amount_fen,o.currency,o.product_id,o.product_snapshot,
 t.status,t.amount_fen,t.currency,p.source_reference
 FROM entitlement_packages p JOIN wallets w ON w.id=p.wallet_id
 JOIN payment_orders o ON o.id=p.payment_order_id AND o.wallet_id=p.wallet_id
 JOIN payment_transactions t ON t.id=p.source_payment_transaction_id AND t.payment_order_id=o.id
 WHERE p.id=$1 AND p.payment_order_id=$2`, packageID, orderID).
		Scan(&walletID, &transactionID, &packageStatus, &sourceType,
			&packageTotal, &remaining, &reserved, &used, &expired, &revoked, &packageUntil,
			&walletStatus, &walletVersion, &orderStatus, &fulfillmentStatus, &orderCredits, &orderAmount,
			&orderCurrency, &productID, &rawProduct, &transactionStatus, &transactionAmount, &transactionCurrency, &sourceReference)
	if errors.Is(err, pgx.ErrNoRows) {
		return entitlementCorrectionReadPlan{}, ErrConflict
	}
	if err != nil {
		return entitlementCorrectionReadPlan{}, err
	}
	if packageStatus != "ACTIVE" || sourceType != "PAYMENT" || walletStatus != "ACTIVE" || walletVersion < 1 ||
		fulfillmentStatus != "CREDITED" ||
		!((orderStatus == "PAID" && transactionStatus == "PAID") || (orderStatus == "PAID_LATE" && transactionStatus == "LATE_PAID")) ||
		packageTotal <= 0 || remaining != packageTotal || reserved != 0 || used != 0 || expired != 0 || revoked != 0 ||
		orderCredits != packageTotal || orderAmount <= 0 || transactionAmount != orderAmount ||
		orderCurrency != "CNY" || transactionCurrency != orderCurrency || !packageUntil.After(r.now()) ||
		sourceReference != "payment-transaction:"+transactionID.String() {
		return entitlementCorrectionReadPlan{}, ErrConflict
	}
	var product Product
	if json.Unmarshal(rawProduct, &product) != nil || product.ProductID != productID ||
		product.CreditMicrocredits != orderCredits || product.AmountFen != orderAmount || product.Currency != orderCurrency ||
		product.BasePoints < 0 || product.BonusPoints < 0 {
		return entitlementCorrectionReadPlan{}, ErrConflict
	}
	var hasRelated bool
	err = tx.QueryRow(bounded, `SELECT
 EXISTS(SELECT 1 FROM member_quota_grants WHERE package_id=$1) OR
 EXISTS(SELECT 1 FROM usage_hold_allocations WHERE package_id=$1) OR
 EXISTS(SELECT 1 FROM refunds WHERE entitlement_package_id=$1) OR
 EXISTS(SELECT 1 FROM forced_refund_cases WHERE entitlement_package_id=$1) OR
 EXISTS(SELECT 1 FROM chargeback_cases WHERE entitlement_package_id=$1) OR
 EXISTS(SELECT 1 FROM receivables WHERE entitlement_package_id=$1) OR
 EXISTS(SELECT 1 FROM entitlement_component_corrections WHERE package_id=$1) OR
 EXISTS(SELECT 1 FROM payment_order_repairs WHERE payment_order_id=$2) OR
 EXISTS(SELECT 1 FROM payment_anomaly_cases WHERE payment_order_id=$2 OR payment_transaction_id=$3)`, packageID, orderID, transactionID).Scan(&hasRelated)
	if err != nil {
		return entitlementCorrectionReadPlan{}, err
	}
	if hasRelated {
		return entitlementCorrectionReadPlan{}, ErrConflict
	}
	var journalID uuid.UUID
	err = tx.QueryRow(bounded, `SELECT id FROM journal_entries WHERE business_reference=$1 AND entry_type='RECHARGE'`, sourceReference).Scan(&journalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return entitlementCorrectionReadPlan{}, ErrConflict
	}
	if err != nil {
		return entitlementCorrectionReadPlan{}, err
	}
	rows, err := tx.Query(bounded, `SELECT c.id,c.component_type,c.credit_lot_id,c.original_microcredits,c.available_microcredits,
 c.reserved_microcredits,c.used_microcredits,c.active_grant_microcredits,c.suspended_grant_microcredits,
 c.expired_microcredits,c.revoked_microcredits,c.reclassified_out_microcredits,c.status,c.expires_at,
 l.wallet_id,l.original_microcredits,l.remaining_microcredits,l.refundable,l.expires_at,l.source_type,l.source_reference
 FROM entitlement_components c JOIN credit_lots l ON l.id=c.credit_lot_id WHERE c.package_id=$1 ORDER BY c.id`, packageID)
	if err != nil {
		return entitlementCorrectionReadPlan{}, err
	}
	var base, bonus int64
	componentIDs := map[string]uuid.UUID{}
	lotIDs := map[string]uuid.UUID{}
	lotAmounts := map[uuid.UUID]int64{}
	count := 0
	for rows.Next() {
		var id, lotID, lotWalletID uuid.UUID
		var kind, status string
		var original, available, reservedComponent, usedComponent, activeGrant, suspendedGrant, expiredComponent, revokedComponent, reclassified, lotOriginal, lotRemaining int64
		var lotSourceType, lotSourceReference string
		var expiresAt, lotExpiresAt time.Time
		var refundable bool
		if err = rows.Scan(&id, &kind, &lotID, &original, &available, &reservedComponent, &usedComponent,
			&activeGrant, &suspendedGrant, &expiredComponent, &revokedComponent, &reclassified, &status, &expiresAt,
			&lotWalletID, &lotOriginal, &lotRemaining, &refundable, &lotExpiresAt, &lotSourceType, &lotSourceReference); err != nil {
			break
		}
		count++
		if count > 2 || (kind != "BASE" && kind != "BONUS") || componentIDs[kind] != uuid.Nil ||
			status != "ACTIVE" || original <= 0 || available != original ||
			reservedComponent != 0 || usedComponent != 0 || activeGrant != 0 || suspendedGrant != 0 ||
			expiredComponent != 0 || revokedComponent != 0 || reclassified != 0 ||
			lotWalletID != walletID || lotOriginal != original || lotRemaining != original ||
			lotSourceType != "RECHARGE" ||
			(lotSourceReference != sourceReference && (kind != "BONUS" || lotSourceReference != sourceReference+":bonus")) ||
			refundable != (kind == "BASE") || !expiresAt.Equal(lotExpiresAt) ||
			!expiresAt.Equal(packageUntil) || !expiresAt.After(r.now()) ||
			original > math.MaxInt64-base-bonus {
			err = ErrConflict
			break
		}
		componentIDs[kind], lotIDs[kind] = id, lotID
		lotAmounts[lotID] = original
		if kind == "BASE" {
			base += original
		} else {
			bonus += original
		}
	}
	if closeErr := rows.Err(); err == nil {
		err = closeErr
	}
	rows.Close()
	if err != nil {
		return entitlementCorrectionReadPlan{}, err
	}
	if count == 0 {
		return entitlementCorrectionReadPlan{}, ErrConflict
	}
	amounts, err := calculateEntitlementCorrectionAmounts(packageTotal, base, bonus, product.BasePoints, product.BonusPoints)
	if err != nil {
		return entitlementCorrectionReadPlan{}, err
	}
	postingRows, err := tx.Query(bounded, `SELECT posting.credit_lot_id,posting.delta_microcredits
 FROM ledger_postings posting JOIN ledger_accounts account ON account.id=posting.ledger_account_id
	 WHERE posting.journal_entry_id=$1 AND account.wallet_id=$2 AND account.account_type='WALLET_LIABILITY'`, journalID, walletID)
	if err != nil {
		return entitlementCorrectionReadPlan{}, err
	}
	seenLots := map[uuid.UUID]bool{}
	for postingRows.Next() {
		var postedLot uuid.UUID
		var delta int64
		if err = postingRows.Scan(&postedLot, &delta); err != nil {
			break
		}
		if postedLot == uuid.Nil || seenLots[postedLot] || lotAmounts[postedLot] != delta {
			err = ErrConflict
			break
		}
		seenLots[postedLot] = true
	}
	if closeErr := postingRows.Err(); err == nil {
		err = closeErr
	}
	postingRows.Close()
	if err != nil {
		return entitlementCorrectionReadPlan{}, err
	}
	if len(seenLots) != len(lotAmounts) {
		return entitlementCorrectionReadPlan{}, ErrConflict
	}
	if err = tx.Commit(bounded); err != nil {
		return entitlementCorrectionReadPlan{}, err
	}
	return entitlementCorrectionReadPlan{
		PackageID: packageID, WalletID: walletID, PaymentOrderID: orderID, PaymentTransactionID: transactionID,
		SourceComponentID: componentIDs[amounts.SourceType], SourceLotID: lotIDs[amounts.SourceType],
		ExistingTargetID: componentIDs[amounts.TargetType], Amounts: amounts,
	}, nil
}
