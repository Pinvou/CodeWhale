package billing

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Wallet serializes every financial writer, including settlement, repayment
// and order projection. Order/source non-key locks preserve FK key-share reads.
// Initial caller lookups are rechecked as the complete source tuple under locks.
func lockPaymentSourcePackage(ctx context.Context, tx pgx.Tx, packageID, transactionID, orderID, walletID uuid.UUID) error {
	if _, err := getWallet(ctx, tx, walletID, true); err != nil {
		return err
	}
	var locked uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM payment_orders WHERE id=$1 AND wallet_id=$2 FOR NO KEY UPDATE`, orderID, walletID).Scan(&locked); errors.Is(err, pgx.ErrNoRows) {
		return ErrConflict
	} else if err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT id FROM payment_transactions WHERE id=$1 AND payment_order_id=$2 AND status IN ('PAID','LATE_PAID') FOR NO KEY UPDATE`, transactionID, orderID).Scan(&locked); errors.Is(err, pgx.ErrNoRows) {
		return ErrConflict
	} else if err != nil {
		return err
	}
	err := tx.QueryRow(ctx, `SELECT id FROM entitlement_packages WHERE id=$1 AND source_payment_transaction_id=$2 AND payment_order_id=$3 AND wallet_id=$4 FOR UPDATE`, packageID, transactionID, orderID, walletID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConflict
	}
	return err
}
