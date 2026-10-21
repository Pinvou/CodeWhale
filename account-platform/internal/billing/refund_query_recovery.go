package billing

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// A possible-dispatch request can only consume a persisted authoritative result
// or query that same original key. Missing/unknown evidence stays in review.
func (p *PaymentCoordinator) recoverDispatchedRefund(ctx context.Context, requested Refund) (Refund, bool, error) {
	tx, err := p.repository.pool.Begin(ctx)
	if err != nil {
		return requested, false, err
	}
	defer tx.Rollback(context.Background())
	order, err := getPaymentOrderByID(ctx, tx, requested.PaymentOrderID, true)
	if err != nil {
		return requested, false, err
	}
	refund, err := getRefundByID(ctx, tx, requested.ID, true)
	if err != nil {
		return requested, false, err
	}
	var dispatch string
	if err := tx.QueryRow(ctx, `SELECT dispatch_state FROM refunds WHERE id=$1`, refund.ID).Scan(&dispatch); err != nil {
		return refund, false, err
	}
	if dispatch == "NOT_STARTED" {
		return refund, false, nil
	}
	if refund.Status != "REQUESTED" && refund.Status != "MANUAL_REVIEW" {
		return refund, true, nil
	}
	if p.Capabilities().ProviderID != order.ProviderID || p.Capabilities().Version != order.ProviderVersion {
		return refund, true, ErrConflict
	}
	// The exact result already persisted for F13 is final evidence. Repeated
	// reconciliation must not mutate its approval snapshot or query externally.
	// Evidence stores this lookup identity with the published TrimSpace rule.
	// Preserve the raw refund/result and nil->SQL NULL for strict validation.
	var evidenceRefundID *string
	if refund.ChannelRefundID != nil {
		canonicalReference := strings.TrimSpace(*refund.ChannelRefundID)
		evidenceRefundID = &canonicalReference
	}
	var raw []byte
	var digest string
	err = tx.QueryRow(ctx, `SELECT payload_snapshot,payload_digest FROM payment_evidence WHERE payment_order_id=$1 AND payment_transaction_id=$2
 AND provider_id=$3 AND provider_version=$4 AND evidence_type='QUERY' AND evidence_key=$5 AND provider_status='SUCCESS'
 AND provider_refund_id=$6 AND amount_fen=$7 AND currency=$8 ORDER BY received_at,id LIMIT 1`, order.ID, refund.PaymentTransactionID,
		order.ProviderID, order.ProviderVersion, "refund:"+refund.ID.String()+":"+refund.MerchantRefundNo, evidenceRefundID, refund.AmountFen, order.Currency).Scan(&raw, &digest)
	if err == nil {
		var result ProviderRefund
		if json.Unmarshal(raw, &result) != nil || !refundQueryResultMatches(refund, result) || result.Status != "SUCCESS" {
			return refund, true, ErrConflict
		}
		canonical, err := json.Marshal(result)
		if err != nil || digestHex(canonical) != digest {
			return refund, true, ErrConflict
		}
		return refund, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return refund, true, err
	}
	reader, ok := p.provider.(payment.RefundQuerier)
	if !ok {
		return refundQueryReview(ctx, tx, refund, p)
	}
	result, queryErr := providerCall(ctx, p, "REFUND", "query", false, providerCallScope(order), func() (ProviderRefund, error) {
		return reader.QueryRefund(ctx, order.MerchantOrderNo, refund.MerchantRefundNo)
	})
	if queryErr != nil || !refundQueryResultMatches(refund, result) || result.Status != "SUCCESS" && result.Status != "FAILED" {
		return refundQueryReview(ctx, tx, refund, p)
	}
	blocked, err := packageFinancialFreezeBlocked(ctx, tx, refund.EntitlementPackageID, order.WalletID, refund.ID, uuid.Nil)
	if err != nil {
		return refund, true, err
	}
	if blocked && result.Status == "SUCCESS" {
		refund, err = p.recordRefundSuccessConflict(ctx, tx, order, refund, result, "competing financial evidence after original provider refund", p.now())
		if err != nil {
			return refund, true, err
		}
		return refund, true, tx.Commit(ctx)
	}
	// Settlement retains the ordinary grant CAS. An authoritative SUCCESS over
	// a prior suspension becomes the existing local-conflict evidence for F13.
	if _, err := tx.Exec(ctx, `UPDATE refunds SET status='REQUESTED' WHERE id=$1 AND status='MANUAL_REVIEW'`, refund.ID); err != nil {
		return refund, true, err
	}
	resultRefund, _, err := p.finishRefundTransaction(ctx, tx, refund, result, false)
	return resultRefund, true, err
}

func refundQueryResultMatches(refund Refund, result ProviderRefund) bool {
	return result.MerchantRefundNo == refund.MerchantRefundNo && result.AmountFen == refund.AmountFen &&
		(refund.ChannelRefundID == nil || strings.TrimSpace(*refund.ChannelRefundID) == "" || *refund.ChannelRefundID == result.ChannelRefundID) &&
		(result.Status != "SUCCESS" || strings.TrimSpace(result.ChannelRefundID) != "")
}

func refundQueryReview(ctx context.Context, tx pgx.Tx, refund Refund, p *PaymentCoordinator) (Refund, bool, error) {
	if refund.Status != "MANUAL_REVIEW" {
		failure := "original refund result unavailable; read-only provider evidence requires review"
		if _, err := tx.Exec(ctx, `UPDATE refunds SET status='MANUAL_REVIEW',dispatch_lease_until=NULL,failure_reason=$2,intent_version=intent_version+1,updated_at=$3 WHERE id=$1`, refund.ID, failure, p.now()); err != nil {
			return refund, true, err
		}
		refund.Status, refund.FailureReason = "MANUAL_REVIEW", &failure
		if _, err := createPaymentAnomalyWithinTransaction(ctx, tx, "REFUND_FAILURE", "P1", "refund:"+refund.ID.String()+":query-unavailable", paymentAnomalyReference{PaymentOrderID: &refund.PaymentOrderID, PaymentTransactionID: &refund.PaymentTransactionID, RefundID: &refund.ID}, map[string]any{"failure_reason": failure}, p.now()); err != nil {
			return refund, true, err
		}
	}
	return refund, true, tx.Commit(ctx)
}
