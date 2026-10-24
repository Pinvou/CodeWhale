package billing

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The immutable Create receipt identifies the original command. Its payment
// instructions are a live projection, so replay must read the current order.
// This read never dispatches a provider call or changes the original receipt.
func (p *PaymentCoordinator) currentPaymentOrderProjection(ctx context.Context, original PaymentOrder) (PaymentOrder, error) {
	current, err := getPaymentOrderByID(ctx, p.repository.pool, original.ID, false)
	if err != nil {
		return PaymentOrder{}, err
	}
	if current.RequesterID != original.RequesterID || current.WalletID != original.WalletID || current.MerchantOrderNo != original.MerchantOrderNo {
		return PaymentOrder{}, ErrConflict
	}
	maskClosedPaymentInstructions(&current, p.now())
	return current, nil
}

// External I/O has finished before this short transaction. Reuse the existing
// wallet -> order lock discipline and decide using the row after acquiring both
// locks, rather than the snapshot taken before the provider query.
func (p *PaymentCoordinator) persistQueriedPaymentProjection(ctx context.Context, orderID uuid.UUID, provider ProviderOrder) (PaymentOrder, error) {
	tx, err := p.repository.pool.Begin(ctx)
	if err != nil {
		return PaymentOrder{}, err
	}
	defer tx.Rollback(ctx)
	order, err := getPaymentOrderByID(ctx, tx, orderID, true)
	if err != nil {
		return PaymentOrder{}, err
	}
	now := p.now()
	providerPending := provider.Status == "CREATED" || provider.Status == "PENDING"
	if err = p.observeQueriedPaymentDeadline(ctx, tx, order, provider, now); err != nil {
		return PaymentOrder{}, err
	}
	var mergeErr error
	if provider.Status != "SUCCESS" {
		mergeErr = p.mergeProviderObservationWithinTransaction(ctx, tx, order, "QUERY", providerEvidenceKey("QUERY", provider), nil, provider, now)
		if mergeErr != nil && !errors.Is(mergeErr, ErrConflict) {
			return PaymentOrder{}, mergeErr
		}
	}
	order, err = getPaymentOrderByID(ctx, tx, order.ID, false)
	if err != nil {
		return PaymentOrder{}, err
	}
	order.PaymentURL, order.QRData = "", ""
	if providerPending && providerMatchesOrder(provider, order) {
		order.PaymentURL, order.QRData = provider.PaymentURL, provider.QRData
	}
	maskClosedPaymentInstructions(&order, p.now())
	if _, err = tx.Exec(ctx, `UPDATE payment_orders SET payment_url=NULLIF($2,''),qr_data=NULLIF($3,''),last_successful_query_at=$4,last_authoritative_unpaid_at=CASE WHEN $5 AND $6 IN ('CLOSED','FAILED') THEN $4 ELSE last_authoritative_unpaid_at END,provider_status=NULLIF($7,''),provider_trace_id=NULLIF($8,''),updated_at=$4 WHERE id=$1`, order.ID, order.PaymentURL, order.QRData, now, p.Capabilities().AuthoritativeUnpaidTerminal, provider.Status, provider.ProviderStatus, provider.ProviderTraceID); err != nil {
		return PaymentOrder{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return PaymentOrder{}, err
	}
	return order, mergeErr
}

// Only the first claimed, validated Create response normalizes the deadline
// before it is exposed to the user. Recovery and query responses cannot freeze
// a later user payment window. The caller holds the wallet/order locks.
func (p *PaymentCoordinator) freezeCreatedPaymentDeadline(ctx context.Context, tx pgx.Tx, order PaymentOrder, provider ProviderOrder, now time.Time) error {
	deadline := providerPaymentDeadline(provider)
	if deadline == nil || (provider.Status != "CREATED" && provider.Status != "PENDING" && provider.Status != "SUCCESS") || !providerMatchesOrder(provider, order) ||
		(order.Status != "CREATED" && order.Status != "PENDING") || order.CloseStatus != "OPEN" || order.ArchiveStatus != "ACTIVE" {
		return nil
	}
	finalArchiveEligibleAt := deadline.Add(time.Duration(p.Capabilities().MaximumLateConfirmationSeconds)*time.Second + 7*24*time.Hour)
	_, err := tx.Exec(ctx, `UPDATE payment_orders SET expires_at=$2,provider_payable_until=$2,final_archive_eligible_at=$3,updated_at=$4 WHERE id=$1`, order.ID, *deadline, finalArchiveEligibleAt, now)
	return err
}

// The user deadline is frozen by Create. A subsequent channel observation may
// stop payment earlier, but cannot reopen or extend that user window. Retain
// the largest known external/archival bound while the order can still require
// unpaid reconciliation: an out-of-order shorter observation cannot authorize
// premature archive. Every exact observation also remains in payment_evidence.
// A matching external deadline remains relevant even when it first arrives
// with a terminal/review observation; that status does not shorten the bound.
func (p *PaymentCoordinator) observeQueriedPaymentDeadline(ctx context.Context, tx pgx.Tx, order PaymentOrder, provider ProviderOrder, now time.Time) error {
	deadline := providerPaymentDeadline(provider)
	if deadline == nil || !providerMatchesOrder(provider, order) ||
		order.ArchiveStatus != "ACTIVE" || order.Status == "PAID" || order.Status == "PAID_LATE" || order.FulfillmentStatus == "CREDITED" {
		return nil
	}
	finalArchiveEligibleAt := deadline.Add(time.Duration(p.Capabilities().MaximumLateConfirmationSeconds)*time.Second + latePaymentReviewGrace)
	_, err := tx.Exec(ctx, `UPDATE payment_orders SET
expires_at=CASE WHEN status IN ('CREATED','PENDING') AND close_status='OPEN' THEN LEAST(expires_at,$2) ELSE expires_at END,
provider_payable_until=GREATEST(provider_payable_until,$2),
final_archive_eligible_at=GREATEST(final_archive_eligible_at,$3),updated_at=$4 WHERE id=$1`, order.ID, *deadline, finalArchiveEligibleAt, now)
	return err
}
