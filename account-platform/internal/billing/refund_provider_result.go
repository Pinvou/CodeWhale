package billing

import (
	"context"

	"github.com/google/uuid"
)

// Refund and read-only recovery share a local result boundary. No database
// transaction is retained by either external call. Reacquire the normal
// wallet/order/refund gate and use current facts before any posting or thaw.
func (p *PaymentCoordinator) finishRefundProviderResult(ctx context.Context, observedOrder PaymentOrder, requested Refund, result ProviderRefund, providerErr error, query bool) (Refund, error) {
	tx, err := p.repository.pool.Begin(ctx)
	if err != nil {
		return requested, err
	}
	defer tx.Rollback(context.Background())
	order, err := getPaymentOrderByID(ctx, tx, observedOrder.ID, true)
	if err != nil {
		return requested, err
	}
	refund, err := getRefundByID(ctx, tx, requested.ID, true)
	if err != nil {
		return requested, err
	}
	if !sameRefundDispatchIdentity(requested, refund) || order.ID != observedOrder.ID || order.WalletID != observedOrder.WalletID ||
		order.RequesterID != observedOrder.RequesterID || order.MerchantOrderNo != observedOrder.MerchantOrderNo || order.Currency != observedOrder.Currency ||
		order.ProviderID != observedOrder.ProviderID || order.ProviderVersion != observedOrder.ProviderVersion ||
		p.Capabilities().ProviderID != order.ProviderID || p.Capabilities().Version != order.ProviderVersion {
		return refund, ErrConflict
	}
	// A concurrent read may already have completed this same original intent.
	// Never let a late error/response downgrade it or post a second journal.
	if refund.Status != "REQUESTED" && refund.Status != "MANUAL_REVIEW" {
		return refund, tx.Commit(ctx)
	}
	if known, err := persistedRefundSuccess(ctx, tx, order, refund); known || err != nil {
		return refund, err
	}
	valid := providerErr == nil && refundQueryResultMatches(refund, result) && (result.Status == "SUCCESS" || result.Status == "FAILED")
	if !valid && query {
		current, _, err := refundQueryReview(ctx, tx, refund, p)
		return current, err
	}
	if providerErr == nil && !refundQueryResultMatches(refund, result) {
		// A previously known external reference is never replaceable by a
		// different ACK. Other identity conflicts retain the ordinary review.
		if refund.Status != "REQUESTED" || (refund.ChannelRefundID != nil && *refund.ChannelRefundID != "" && *refund.ChannelRefundID != result.ChannelRefundID) {
			return refund, ErrConflict
		}
		current, _, err := p.finishRefundTransaction(ctx, tx, refund, result, false)
		return current, err
	}
	if providerErr != nil || result.Status != "SUCCESS" && result.Status != "FAILED" {
		if _, err := tx.Exec(ctx, `UPDATE refunds SET dispatch_state='RESULT_UNKNOWN',dispatch_lease_until=NULL WHERE id=$1 AND dispatch_state='IN_PROGRESS'`, refund.ID); err != nil {
			return refund, err
		}
		if err := tx.Commit(ctx); err != nil {
			return refund, err
		}
		return refund, ErrPaymentProvider
	}
	blocked, err := packageFinancialFreezeBlocked(ctx, tx, refund.EntitlementPackageID, order.WalletID, refund.ID, uuid.Nil)
	if err != nil {
		return refund, err
	}
	if blocked && result.Status == "SUCCESS" {
		current, err := p.recordRefundSuccessConflict(ctx, tx, order, refund, result, "competing financial evidence after original provider refund", p.now())
		if err != nil {
			return refund, err
		}
		return current, tx.Commit(ctx)
	}
	// A trusted terminal result may close the same unknown original intent.
	// Existing settlement still owns reservation CAS, snapshot validation,
	// journals, conservation, failure thaw checks, outbox and final receipt.
	if _, err := tx.Exec(ctx, `UPDATE refunds SET status='REQUESTED' WHERE id=$1 AND status='MANUAL_REVIEW'`, refund.ID); err != nil {
		return refund, err
	}
	current, _, err := p.finishRefundTransaction(ctx, tx, refund, result, false)
	return current, err
}

func sameRefundDispatchIdentity(a, b Refund) bool {
	return a.ID == b.ID && a.PaymentOrderID == b.PaymentOrderID && a.PaymentTransactionID == b.PaymentTransactionID &&
		a.EntitlementPackageID == b.EntitlementPackageID && a.RequesterID == b.RequesterID && a.MerchantRefundNo == b.MerchantRefundNo &&
		a.AmountFen == b.AmountFen && a.CreditMicrocredits == b.CreditMicrocredits && a.BaseMicrocredits == b.BaseMicrocredits &&
		a.BonusMicrocredits == b.BonusMicrocredits && a.Reason == b.Reason
}
