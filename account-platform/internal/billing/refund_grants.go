package billing

import (
	"context"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Ordinary dispatch may only revoke grants suspended by this refund. A legacy
// reservation can contain an earlier administrative/account-closure suspension;
// that evidence belongs to governed repair, never a new provider request.
func ordinaryRefundReservationsEligible(ctx context.Context, source queryable, refundID uuid.UUID) (bool, error) {
	var eligible bool
	err := source.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM refund_component_reservations WHERE refund_id=$1)
 AND (SELECT COALESCE(sum(available_microcredits+active_grant_microcredits+suspended_grant_microcredits),0)
 FROM refund_component_reservations WHERE refund_id=$1)=(SELECT credit_microcredits FROM refunds WHERE id=$1)
 AND NOT EXISTS(SELECT 1 FROM refund_component_reservations reservation
 JOIN refunds refund ON refund.id=reservation.refund_id
 LEFT JOIN entitlement_components component ON component.id=reservation.component_id
 WHERE refund.id=$1 AND (component.id IS NULL OR reservation.package_id<>refund.entitlement_package_id
 OR component.package_id<>reservation.package_id OR component.credit_lot_id<>reservation.credit_lot_id
 OR component.component_type<>reservation.component_type OR reservation.prior_status<>'ACTIVE'
 OR component.status<>'FROZEN' OR component.available_microcredits<>reservation.available_microcredits
 OR component.active_grant_microcredits<>reservation.active_grant_microcredits
 OR component.suspended_grant_microcredits<>reservation.suspended_grant_microcredits
 OR component.used_microcredits<>0 OR component.reserved_microcredits<>0 OR component.expired_microcredits<>0))
 AND NOT EXISTS(SELECT 1 FROM entitlement_components component JOIN refunds refund ON refund.entitlement_package_id=component.package_id
 WHERE refund.id=$1 AND NOT EXISTS(SELECT 1 FROM refund_component_reservations reservation WHERE reservation.refund_id=refund.id AND reservation.component_id=component.id))
 AND NOT EXISTS(
 SELECT 1 FROM refund_grant_reservations reservation
 LEFT JOIN member_quota_grants quota_grant ON quota_grant.id=reservation.grant_id
 WHERE reservation.refund_id=$1 AND (
 quota_grant.id IS NULL OR reservation.prior_status<>'ACTIVE' OR reservation.prior_suspension_reason IS NOT NULL
 OR quota_grant.status<>'SUSPENDED' OR quota_grant.suspension_reason IS DISTINCT FROM 'REFUND_PENDING'
 OR quota_grant.component_id<>reservation.component_id
 OR quota_grant.available_microcredits<>reservation.available_microcredits
 OR quota_grant.used_microcredits<>0 OR quota_grant.reserved_microcredits<>0
 OR quota_grant.expired_microcredits<>0 OR quota_grant.revoked_microcredits<>0))
 AND NOT EXISTS(SELECT 1 FROM member_quota_grants quota_grant
 JOIN refunds refund ON refund.entitlement_package_id=quota_grant.package_id
 WHERE refund.id=$1 AND NOT EXISTS(SELECT 1 FROM refund_grant_reservations reservation
 WHERE reservation.refund_id=refund.id AND reservation.grant_id=quota_grant.id))`, refundID).Scan(&eligible)
	return eligible, err
}

// This preflight precedes the durable possible-dispatch claim. The ordinary
// financial transaction repeats the check under the wallet/order/refund gate,
// so a concurrent lifecycle change cannot race an external call.
func (p *PaymentCoordinator) refundDispatchGrantPreflight(ctx context.Context, requested Refund, reconcile bool) (Refund, bool, error) {
	tx, err := p.repository.pool.Begin(ctx)
	if err != nil {
		return requested, false, err
	}
	defer tx.Rollback(context.Background())
	if _, err := getPaymentOrderByID(ctx, tx, requested.PaymentOrderID, true); err != nil {
		return requested, false, err
	}
	refund, err := getRefundByID(ctx, tx, requested.ID, true)
	if err != nil {
		return requested, false, err
	}
	if refund.Status != "REQUESTED" && !(reconcile && refund.Status == "MANUAL_REVIEW") {
		return refund, false, tx.Commit(ctx)
	}
	eligible, err := ordinaryRefundReservationsEligible(ctx, tx, refund.ID)
	if err != nil {
		return refund, false, err
	}
	if !eligible {
		var dispatch string
		if err := tx.QueryRow(ctx, `SELECT dispatch_state FROM refunds WHERE id=$1`, refund.ID).Scan(&dispatch); err != nil {
			return refund, false, err
		}
		// A possible earlier dispatch needs its authoritative result first.
		// Creating a second eligibility anomaly here would outlive the existing
		// local-conflict repair that owns a confirmed SUCCESS.
		if dispatch == "NOT_STARTED" {
			refund, err = markRefundGrantReview(ctx, tx, refund, p.now())
			if err != nil {
				return refund, false, err
			}
		}
	}
	return refund, eligible, tx.Commit(ctx)
}

func markRefundGrantReview(ctx context.Context, tx pgx.Tx, refund Refund, now time.Time) (Refund, error) {
	failure := "refund grant eligibility requires governed financial review"
	if refund.Status == "MANUAL_REVIEW" {
		return refund, nil
	}
	// Retain NOT_STARTED for a request never dispatched. An old unknown boundary
	// stays unknown; an eligibility refusal is not evidence of a provider call.
	if _, err := tx.Exec(ctx, `UPDATE refunds SET status='MANUAL_REVIEW',dispatch_lease_until=NULL,
 failure_reason=$2,intent_version=intent_version+1,updated_at=$3 WHERE id=$1`, refund.ID, failure, now); err != nil {
		return refund, err
	}
	if _, err := createPaymentAnomalyWithinTransaction(ctx, tx, "REFUND_FAILURE", "P1", "refund:"+refund.ID.String()+":grant-ineligible",
		paymentAnomalyReference{PaymentOrderID: &refund.PaymentOrderID, PaymentTransactionID: &refund.PaymentTransactionID, RefundID: &refund.ID},
		map[string]any{"failure_reason": failure}, now); err != nil {
		return refund, err
	}
	refund.Status, refund.FailureReason, refund.UpdatedAt = "MANUAL_REVIEW", &failure, now
	return refund, nil
}

// A matched authoritative SUCCESS must survive a local eligibility refusal.
// This is the same original-refund evidence and escalation identity used by
// the existing local-balance-conflict repair, without posting any funds.
func (p *PaymentCoordinator) recordRefundSuccessConflict(ctx context.Context, tx pgx.Tx, order PaymentOrder, refund Refund, provider ProviderRefund, failure string, now time.Time) (Refund, error) {
	if _, err := tx.Exec(ctx, `UPDATE refunds SET dispatch_state='RESULT_UNKNOWN',dispatch_lease_until=NULL,status='MANUAL_REVIEW',failure_reason=$2,channel_refund_id=$3,intent_version=intent_version+1,updated_at=$4 WHERE id=$1`, refund.ID, failure, provider.ChannelRefundID, now); err != nil {
		return refund, err
	}
	if err := events.AppendOutbox(ctx, tx, "billing.refund.review-required.v1", refund.ID, 2, map[string]any{"refund_id": refund.ID, "payment_transaction_id": refund.PaymentTransactionID, "entitlement_package_id": refund.EntitlementPackageID}); err != nil {
		return refund, err
	}
	if _, err := p.insertRefundSuccessEvidence(ctx, tx, order, refund, provider, now); err != nil {
		return refund, err
	}
	if _, err := createPaymentAnomalyWithinTransaction(ctx, tx, "REFUND_FAILURE", "P0", "refund:"+refund.ID.String()+":local-conflict",
		paymentAnomalyReference{PaymentOrderID: &refund.PaymentOrderID, PaymentTransactionID: &refund.PaymentTransactionID, RefundID: &refund.ID},
		map[string]any{"failure_reason": failure, "provider_refund_id": provider.ChannelRefundID}, now); err != nil {
		return refund, err
	}
	refund.Status, refund.ChannelRefundID, refund.FailureReason, refund.UpdatedAt = "MANUAL_REVIEW", &provider.ChannelRefundID, &failure, now
	return refund, nil
}
