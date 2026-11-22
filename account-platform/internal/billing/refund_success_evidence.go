package billing

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"strings"
	"time"
)

// insertRefundSuccessEvidence records a verified result from the original
// idempotent provider when local settlement conflicts. It performs no provider
// call and shares the transaction that preserves MANUAL_REVIEW/RESULT_UNKNOWN.
func (p *PaymentCoordinator) insertRefundSuccessEvidence(ctx context.Context, tx pgx.Tx, order PaymentOrder, refund Refund, result ProviderRefund, now time.Time) (uuid.UUID, error) {
	if result.Status != "SUCCESS" || strings.TrimSpace(result.ChannelRefundID) == "" || result.MerchantRefundNo != refund.MerchantRefundNo || result.AmountFen != refund.AmountFen || refund.PaymentOrderID != order.ID || refund.PaymentTransactionID == uuid.Nil {
		return uuid.Nil, ErrConflict
	}
	var providerID, version, transactionRef string
	if err := tx.QueryRow(ctx, `SELECT provider_id,provider_version,provider_transaction_id FROM payment_transactions WHERE id=$1 AND payment_order_id=$2`, refund.PaymentTransactionID, order.ID).Scan(&providerID, &version, &transactionRef); err != nil {
		return uuid.Nil, err
	}
	if providerID != order.ProviderID || version != order.ProviderVersion {
		return uuid.Nil, ErrConflict
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return uuid.Nil, err
	}
	digest := digestHex(payload)
	key := "refund:" + refund.ID.String() + ":" + refund.MerchantRefundNo
	id := uuid.Must(uuid.NewV7())
	tag, err := tx.Exec(ctx, `INSERT INTO payment_evidence(id,payment_order_id,payment_transaction_id,evidence_type,provider_id,provider_version,evidence_key,merchant_order_no,provider_transaction_id,provider_refund_id,amount_fen,currency,provider_status,received_at,payload_digest,payload_snapshot,merge_result)
 VALUES($1,$2,$3,'QUERY',$4,$5,$6,$7,$8,$9,$10,$11,'SUCCESS',$12,$13,$14,'REVIEW_REQUIRED') ON CONFLICT(provider_id,provider_version,evidence_type,evidence_key,payload_digest) DO NOTHING`, id, order.ID, refund.PaymentTransactionID, providerID, version, key, order.MerchantOrderNo, transactionRef, strings.TrimSpace(result.ChannelRefundID), result.AmountFen, order.Currency, now, digest, payload)
	if err != nil {
		return uuid.Nil, err
	}
	if tag.RowsAffected() == 0 {
		err = tx.QueryRow(ctx, `SELECT id FROM payment_evidence WHERE provider_id=$1 AND provider_version=$2 AND evidence_type='QUERY' AND evidence_key=$3 AND payload_digest=$4`, providerID, version, key, digest).Scan(&id)
	}
	return id, err
}
