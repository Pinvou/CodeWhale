package billing

import (
	"context"

	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type ExpiredPaymentCandidate struct {
	OrderID              uuid.UUID
	Attempts             int
	LateReviewBoundaryAt time.Time
}

type ExpiredPaymentStore interface {
	ClaimExpiredPaymentOrders(context.Context, int, time.Duration) ([]ExpiredPaymentCandidate, error)
	RescheduleExpiredPaymentOrder(context.Context, uuid.UUID, string, time.Time) error
	EscalateLatePaymentReview(context.Context, uuid.UUID, string) (LatePaymentReviewCase, error)
}

type PaymentOrderSynchronizer interface {
	SyncOrder(context.Context, uuid.UUID) (PaymentOrder, error)
}

type ExpiredPaymentReconciler struct {
	store        ExpiredPaymentStore
	synchronizer PaymentOrderSynchronizer
	interval     time.Duration
	lease        time.Duration
	batchSize    int
	now          func() time.Time
	logger       *slog.Logger
}

func NewExpiredPaymentReconciler(store ExpiredPaymentStore, synchronizer PaymentOrderSynchronizer, interval time.Duration) *ExpiredPaymentReconciler {
	if interval < time.Second {
		interval = 15 * time.Second
	}
	return &ExpiredPaymentReconciler{
		store:        store,
		synchronizer: synchronizer,
		interval:     interval,
		lease:        5 * time.Minute,
		batchSize:    20,
		now:          func() time.Time { return time.Now().UTC() },
		logger:       slog.Default().With("component", "expired-payment-reconciler"),
	}
}

func (r *ExpiredPaymentReconciler) Run(ctx context.Context) {
	r.runOnce(ctx)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.runOnce(ctx)
		}
	}
}

func (r *ExpiredPaymentReconciler) runOnce(ctx context.Context) {
	items, err := r.store.ClaimExpiredPaymentOrders(ctx, r.batchSize, r.lease)
	if err != nil {
		r.logger.Error("claim expired payment orders failed", "error", err)
		return
	}
	for _, item := range items {
		order, syncErr := r.synchronizer.SyncOrder(ctx, item.OrderID)
		if syncErr == nil && (order.Status == "CREATED" || order.Status == "PENDING" || order.Status == "EXPIRED") {
			if !item.LateReviewBoundaryAt.After(r.now()) {
				if _, err := r.store.EscalateLatePaymentReview(ctx, item.OrderID, "PENDING"); err != nil {
					r.logger.Error("escalate late payment review failed", "order_id", item.OrderID, "error", err)
				}
				continue
			}
			// Providers without notifications converge through server-side polling,
			// including after the client closes the QR page. A non-terminal state is
			// expected here and is rescheduled rather than treated as an error.
			delay := r.interval
			if order.Status == "EXPIRED" && delay < 5*time.Minute {
				delay = 5 * time.Minute
			}
			nextAt := r.now().Add(delay)
			if err := r.store.RescheduleExpiredPaymentOrder(ctx, item.OrderID, "", nextAt); err != nil {
				r.logger.Error("reschedule pending payment order failed", "order_id", item.OrderID, "error", err)
			}
			continue
		}
		if syncErr == nil {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		retryReason := safePaymentReconciliationError(syncErr)
		if !item.LateReviewBoundaryAt.After(r.now()) {
			if _, err := r.store.EscalateLatePaymentReview(ctx, item.OrderID, retryReason); err != nil {
				r.logger.Error("escalate late payment review after boundary query failed", "order_id", item.OrderID, "error", err)
			}
			continue
		}
		delay := paymentReconciliationBackoff(item.Attempts)
		if delay < r.interval {
			delay = r.interval
		}
		nextAt := r.now().Add(delay)
		if err := r.store.RescheduleExpiredPaymentOrder(ctx, item.OrderID, retryReason, nextAt); err != nil {
			r.logger.Error("reschedule expired payment order failed", "order_id", item.OrderID, "error", err)
			continue
		}
		r.logger.Warn("expired payment order reconciliation deferred", "order_id", item.OrderID, "attempt", item.Attempts, "reason", retryReason)
	}
}

func safePaymentReconciliationError(err error) string {
	switch {
	case errors.Is(err, ErrPaymentProvider):
		return "payment provider unavailable"
	case errors.Is(err, ErrConflict):
		return "payment provider state conflict"
	default:
		return "payment order synchronization failed"
	}
}

func paymentReconciliationBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := 15 * time.Second
	for i := 1; i < attempt && delay < 5*time.Minute; i++ {
		delay *= 2
	}
	if delay > 5*time.Minute {
		return 5 * time.Minute
	}
	return delay
}

func (r *PostgresRepository) ClaimExpiredPaymentOrders(ctx context.Context, limit int, lease time.Duration) ([]ExpiredPaymentCandidate, error) {
	if limit < 1 || limit > 100 || lease < 30*time.Second || lease > 10*time.Minute {
		return nil, ErrConflict
	}
	now := r.now()
	rows, err := r.pool.Query(ctx, `
WITH candidates AS (
    SELECT id
    FROM payment_orders
    WHERE status IN ('CREATED','PENDING','EXPIRED')
      AND (expires_at <= $1 OR provider_sku IS NOT NULL)
      AND (reconciliation_next_at IS NULL OR reconciliation_next_at <= $1)
      AND (reconciliation_lease_until IS NULL OR reconciliation_lease_until <= $1)
    ORDER BY COALESCE(reconciliation_next_at, expires_at), expires_at, id
    FOR UPDATE SKIP LOCKED
    LIMIT $2
)
UPDATE payment_orders payment
SET reconciliation_attempts = payment.reconciliation_attempts + 1,
    reconciliation_lease_until = $3,
    reconciliation_last_error = NULL,
    updated_at = $1
FROM candidates
WHERE payment.id = candidates.id
RETURNING payment.id, payment.reconciliation_attempts, payment.late_review_boundary_at`, now, limit, now.Add(lease))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]ExpiredPaymentCandidate, 0)
	for rows.Next() {
		var item ExpiredPaymentCandidate
		if err := rows.Scan(&item.OrderID, &item.Attempts, &item.LateReviewBoundaryAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) RescheduleExpiredPaymentOrder(ctx context.Context, orderID uuid.UUID, lastError string, nextAt time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE payment_orders
SET reconciliation_next_at=$2,reconciliation_lease_until=NULL,reconciliation_last_error=NULLIF($3,''),updated_at=$4
WHERE id=$1 AND status IN ('CREATED','PENDING','EXPIRED')`, orderID, nextAt.UTC(), truncatePaymentReconciliationError(lastError), r.now())
	return err
}

func truncatePaymentReconciliationError(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 500 {
		return value[:500]
	}
	return value
}

func (p *PaymentCoordinator) SyncOrder(ctx context.Context, orderID uuid.UUID) (PaymentOrder, error) {
	return p.syncOrder(ctx, orderID, false)
}

func (p *PaymentCoordinator) SyncReviewedOrder(ctx context.Context, orderID uuid.UUID) (PaymentOrder, error) {
	return p.syncOrder(ctx, orderID, true)
}

func providerFailureMatchesOrder(provider ProviderOrder, order PaymentOrder) bool {
	return provider.Status == "REVIEW_REQUIRED" && strings.EqualFold(strings.TrimSpace(provider.ProviderStatus), "FAILED") &&
		provider.MerchantOrderNo == order.MerchantOrderNo && provider.Channel == order.Channel &&
		provider.AmountFen == order.AmountFen && provider.Currency == order.Currency &&
		strings.TrimSpace(provider.ExternalCustomerKey) != "" && provider.ExternalCustomerKey == order.RequesterID.String() &&
		strings.TrimSpace(provider.ExternalSKU) != "" && provider.ExternalSKU == order.ProviderSKU &&
		provider.ChannelTransactionID == nil && provider.PaidAt == nil
}

func (p *PaymentCoordinator) ConfirmProviderFailed(ctx context.Context, caseID, actorID uuid.UUID, idempotencyKey, reason string) (PaymentAnomalyCase, error) {
	if !p.Capabilities().OrderQuerySupported || caseID == uuid.Nil || actorID == uuid.Nil || len([]rune(strings.TrimSpace(reason))) < 10 {
		return PaymentAnomalyCase{}, ErrConflict
	}
	detail, err := p.repository.PaymentAnomalyDetail(ctx, caseID)
	if err != nil {
		return PaymentAnomalyCase{}, err
	}
	if detail.Case.AnomalyType != "ILLEGAL_TRANSITION" || detail.Case.Status != "OPEN" || detail.Case.PaymentOrderID == nil || detail.Case.OwnerID == nil || *detail.Case.OwnerID != actorID {
		return PaymentAnomalyCase{}, ErrConflict
	}
	order, err := getPaymentOrderByID(ctx, p.repository.pool, *detail.Case.PaymentOrderID, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentAnomalyCase{}, ErrNotFound
	}
	if err != nil {
		return PaymentAnomalyCase{}, err
	}
	capabilities := p.Capabilities()
	if order.ProviderID != capabilities.ProviderID || order.ProviderVersion != capabilities.Version || order.Status != "MANUAL_REVIEW" {
		return PaymentAnomalyCase{}, ErrConflict
	}
	providerOrder, err := providerCall(ctx, p, "ORDER_QUERY", order.Channel, false, providerCallScope(order), func() (ProviderOrder, error) {
		return p.provider.Get(ctx, order.MerchantOrderNo)
	})
	if err != nil {
		return PaymentAnomalyCase{}, errors.Join(ErrPaymentProvider, err)
	}
	queriedAt := p.now()
	if !providerFailureMatchesOrder(providerOrder, order) {
		return PaymentAnomalyCase{}, ErrConflict
	}
	return p.repository.ConfirmProviderFailed(ctx, caseID, actorID, idempotencyKey, reason, detail.Case.Version, queriedAt, providerOrder)
}

func (p *PaymentCoordinator) syncOrder(ctx context.Context, orderID uuid.UUID, reviewed bool) (PaymentOrder, error) {
	if !p.Capabilities().OrderQuerySupported {
		return PaymentOrder{}, payment.ErrUnsupported
	}
	order, err := getPaymentOrderByID(ctx, p.repository.pool, orderID, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentOrder{}, ErrNotFound
	}
	if err != nil {
		return PaymentOrder{}, err
	}
	if p.Capabilities().ProviderID != order.ProviderID || p.Capabilities().Version != order.ProviderVersion {
		return PaymentOrder{}, ErrConflict
	}
	providerOrder, err := providerCall(ctx, p, "ORDER_QUERY", order.Channel, false, providerCallScope(order), func() (ProviderOrder, error) {
		return p.provider.Get(ctx, order.MerchantOrderNo)
	})
	if err != nil {
		return PaymentOrder{}, errors.Join(ErrPaymentProvider, err)
	}
	if _, err := p.repository.pool.Exec(ctx, `UPDATE payment_orders SET last_successful_query_at=$2,last_authoritative_unpaid_at=CASE WHEN $3 AND $4 IN ('CLOSED','FAILED') THEN $2 ELSE last_authoritative_unpaid_at END,updated_at=$2 WHERE id=$1`, order.ID, p.now(), p.Capabilities().AuthoritativeUnpaidTerminal, providerOrder.Status); err != nil {
		return PaymentOrder{}, err
	}
	if deadline := providerPaymentDeadline(providerOrder); deadline != nil && order.CloseStatus == "OPEN" {
		finalArchiveEligibleAt := deadline.Add(time.Duration(p.Capabilities().MaximumLateConfirmationSeconds)*time.Second + 7*24*time.Hour)
		command, err := p.repository.pool.Exec(ctx, `UPDATE payment_orders SET expires_at=$2,provider_payable_until=$2,final_archive_eligible_at=$3,updated_at=$4 WHERE id=$1 AND close_status='OPEN'`, order.ID, *deadline, finalArchiveEligibleAt, p.now())
		if err != nil {
			return PaymentOrder{}, err
		}
		if command.RowsAffected() == 1 {
			order.ExpiresAt = *deadline
			order.ProviderPayableUntil = *deadline
			order.FinalArchiveEligibleAt = finalArchiveEligibleAt
		}
	}
	if _, err := p.repository.pool.Exec(ctx, `UPDATE payment_orders SET payment_url=CASE WHEN close_status='OPEN' THEN NULLIF($2,'') END,qr_data=CASE WHEN close_status='OPEN' THEN NULLIF($3,'') END,provider_status=NULLIF($4,''),provider_trace_id=NULLIF($5,''),updated_at=$6 WHERE id=$1`, order.ID, providerOrder.PaymentURL, providerOrder.QRData, providerOrder.ProviderStatus, providerOrder.ProviderTraceID, p.now()); err != nil {
		return PaymentOrder{}, err
	}
	order.PaymentURL, order.QRData = providerOrder.PaymentURL, providerOrder.QRData
	maskClosedPaymentInstructions(&order)
	if providerOrder.Status == "SUCCESS" {
		return p.applyQueriedPayment(ctx, order.ID, providerOrder, reviewed)
	}
	if err := p.recordProviderObservation(ctx, order.ID, "QUERY", providerOrder); err != nil {
		return PaymentOrder{}, err
	}
	if providerOrder.Status == "PARTIALLY_REFUNDED" || providerOrder.Status == "REFUNDED" {
		return p.applyProviderReviewStatus(ctx, order, providerOrder.Status)
	}
	if providerOrder.Status == "CLOSED" || providerOrder.Status == "FAILED" {
		if !p.Capabilities().AuthoritativeUnpaidTerminal {
			return order, nil
		}
		return p.applyProviderTerminalStatus(ctx, order, providerOrder.Status)
	}
	if shouldCloseExpiredPaymentOrder(order, providerOrder, p.now()) {
		locallyClosed, err := p.markExpiredOrderLocallyClosed(ctx, order.ID)
		if err != nil {
			return PaymentOrder{}, err
		}
		if !p.Capabilities().CloseSupported {
			return locallyClosed, nil
		}
		closed, closeErr := providerCall(ctx, p, "ORDER_CLOSE", order.Channel, true, providerCallScope(order), func() (ProviderOrder, error) {
			return p.provider.Close(ctx, order.MerchantOrderNo)
		})
		if closeErr != nil {
			return PaymentOrder{}, errors.Join(ErrPaymentProvider, closeErr)
		}
		if closed.MerchantOrderNo != order.MerchantOrderNo || closed.Channel != order.Channel || closed.AmountFen != order.AmountFen || closed.Currency != order.Currency {
			_ = p.recordProviderObservation(ctx, order.ID, "QUERY", closed)
			return PaymentOrder{}, ErrConflict
		}
		switch closed.Status {
		case "SUCCESS":
			return p.applyQueriedPayment(ctx, order.ID, closed, reviewed)
		case "PARTIALLY_REFUNDED", "REFUNDED":
			if err := p.recordProviderObservation(ctx, order.ID, "QUERY", closed); err != nil {
				return PaymentOrder{}, err
			}
			return p.applyProviderReviewStatus(ctx, order, closed.Status)
		case "CLOSED", "FAILED":
			if err := p.recordProviderObservation(ctx, order.ID, "QUERY", closed); err != nil {
				return PaymentOrder{}, err
			}
			if !p.Capabilities().AuthoritativeUnpaidTerminal {
				return locallyClosed, nil
			}
			return p.applyProviderTerminalStatus(ctx, order, closed.Status)
		case "CREATED", "PENDING":
			if err := p.recordProviderObservation(ctx, order.ID, "QUERY", closed); err != nil {
				return PaymentOrder{}, err
			}
			return locallyClosed, nil
		default:
			if err := p.recordProviderObservation(ctx, order.ID, "QUERY", closed); err != nil {
				return PaymentOrder{}, err
			}
			return p.applyProviderReviewStatus(ctx, order, closed.Status)
		}
	}
	if providerOrder.Status != "PENDING" && providerOrder.Status != "CREATED" {
		return p.applyProviderReviewStatus(ctx, order, providerOrder.Status)
	}
	return order, nil
}

func shouldCloseExpiredPaymentOrder(order PaymentOrder, provider ProviderOrder, now time.Time) bool {
	return (order.Status == "CREATED" || order.Status == "PENDING") &&
		(order.CloseStatus == "OPEN" || order.CloseStatus == "CLOSE_REQUESTED" || order.CloseStatus == "LOCALLY_CLOSED_EXTERNAL_PENDING") &&
		!order.ExpiresAt.After(now) && (provider.Status == "CREATED" || provider.Status == "PENDING")
}

func providerPaymentDeadline(provider ProviderOrder) *time.Time {
	if provider.ProviderPayableUntil != nil && !provider.ProviderPayableUntil.IsZero() {
		deadline := provider.ProviderPayableUntil.UTC()
		return &deadline
	}
	if provider.PaymentInstructionExpiresAt != nil && !provider.PaymentInstructionExpiresAt.IsZero() {
		deadline := provider.PaymentInstructionExpiresAt.UTC()
		return &deadline
	}
	return nil
}

func (p *PaymentCoordinator) markExpiredOrderLocallyClosed(ctx context.Context, orderID uuid.UUID) (PaymentOrder, error) {
	tx, err := p.repository.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return PaymentOrder{}, err
	}
	defer tx.Rollback(ctx)
	order, err := getPaymentOrderByID(ctx, tx, orderID, true)
	if err != nil {
		return PaymentOrder{}, err
	}
	now := p.now()
	if order.Status != "CREATED" && order.Status != "PENDING" {
		return order, tx.Commit(ctx)
	}
	if order.ExpiresAt.After(now) {
		return PaymentOrder{}, ErrConflict
	}
	if order.CloseStatus == "OPEN" {
		if _, err := tx.Exec(ctx, `UPDATE payment_orders SET close_status='CLOSE_REQUESTED',close_requested_at=COALESCE(close_requested_at,$2),payment_url=NULL,qr_data=NULL,attention_summary='PROCESSING',summary_version=summary_version+1,updated_at=$2 WHERE id=$1 AND status IN ('CREATED','PENDING') AND close_status='OPEN'`, order.ID, now); err != nil {
			return PaymentOrder{}, err
		}
		order.CloseStatus = "CLOSE_REQUESTED"
	}
	if order.CloseStatus == "CLOSE_REQUESTED" {
		if _, err := tx.Exec(ctx, `UPDATE payment_orders SET status='EXPIRED',close_status='LOCALLY_CLOSED_EXTERNAL_PENDING',locally_closed_at=COALESCE(locally_closed_at,$2),payment_url=NULL,qr_data=NULL,attention_summary='NORMAL',summary_version=summary_version+1,updated_at=$2 WHERE id=$1 AND status IN ('CREATED','PENDING') AND close_status='CLOSE_REQUESTED'`, order.ID, now); err != nil {
			return PaymentOrder{}, err
		}
	} else if order.CloseStatus == "LOCALLY_CLOSED_EXTERNAL_PENDING" {
		if _, err := tx.Exec(ctx, `UPDATE payment_orders SET status='EXPIRED',payment_url=NULL,qr_data=NULL,attention_summary='NORMAL',summary_version=summary_version+1,updated_at=$2 WHERE id=$1 AND status IN ('CREATED','PENDING') AND close_status='LOCALLY_CLOSED_EXTERNAL_PENDING'`, order.ID, now); err != nil {
			return PaymentOrder{}, err
		}
	}
	updated, err := getPaymentOrderByID(ctx, tx, order.ID, false)
	if err != nil {
		return PaymentOrder{}, err
	}
	maskClosedPaymentInstructions(&updated)
	return updated, tx.Commit(ctx)
}

func (p *PaymentCoordinator) applyProviderReviewStatus(ctx context.Context, order PaymentOrder, providerStatus string) (PaymentOrder, error) {
	tx, err := p.repository.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return PaymentOrder{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "payment-order.illegal-transition", order.ID.String()); err != nil {
		return PaymentOrder{}, err
	}
	now := p.now()
	_, err = tx.Exec(ctx, `UPDATE payment_orders
SET status=CASE WHEN status IN ('CREATED','PENDING','EXPIRED') AND fulfillment_status='PENDING' THEN 'MANUAL_REVIEW' ELSE status END,
	fulfillment_status=CASE WHEN status IN ('CREATED','PENDING','EXPIRED') AND fulfillment_status='PENDING' THEN 'MANUAL_REVIEW' ELSE fulfillment_status END,
    attention_summary='REVIEW_REQUIRED',summary_version=summary_version+1,reconciliation_next_at=NULL,reconciliation_lease_until=NULL,reconciliation_last_error=NULL,updated_at=$2
WHERE id=$1`, order.ID, now)
	if err != nil {
		return PaymentOrder{}, err
	}
	if _, err := createPaymentAnomalyWithinTransaction(ctx, tx, "ILLEGAL_TRANSITION", "P0", "order:"+order.ID.String()+":provider-status:"+strings.ToUpper(strings.TrimSpace(providerStatus)), paymentAnomalyReference{PaymentOrderID: &order.ID}, map[string]any{"provider_status": providerStatus, "previous_order_status": order.Status, "previous_fulfillment_status": order.FulfillmentStatus}, now); err != nil {
		return PaymentOrder{}, err
	}
	updated, err := getPaymentOrderByID(ctx, tx, order.ID, false)
	if err != nil {
		return PaymentOrder{}, err
	}
	return updated, tx.Commit(ctx)
}

func (p *PaymentCoordinator) applyProviderTerminalStatus(ctx context.Context, order PaymentOrder, status string) (PaymentOrder, error) {
	if status != "CLOSED" && status != "FAILED" {
		return PaymentOrder{}, fmt.Errorf("unsupported provider terminal status %q", status)
	}
	tx, err := p.repository.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return PaymentOrder{}, err
	}
	defer tx.Rollback(ctx)
	current, err := getPaymentOrderByID(ctx, tx, order.ID, true)
	if err != nil {
		return PaymentOrder{}, err
	}
	// The query ran before this row lock. A concurrent payment or review may
	// already have advanced the order, so unpaid terminal observations cannot
	// replace confirmed payment or review facts.
	if current.Status != "CREATED" && current.Status != "PENDING" && current.Status != "EXPIRED" {
		return current, tx.Commit(ctx)
	}
	now := p.now()
	if status == "FAILED" {
		if current.CloseStatus != "EXPIRED" {
			_, err = tx.Exec(ctx, `UPDATE payment_orders SET status=CASE WHEN status='EXPIRED' THEN 'EXPIRED' ELSE 'FAILED' END,close_status='EXPIRED',payment_url=NULL,qr_data=NULL,reconciliation_next_at=NULL,reconciliation_lease_until=NULL,reconciliation_last_error=NULL,summary_version=summary_version+1,updated_at=$2 WHERE id=$1 AND close_status IN ('OPEN','CLOSE_REQUESTED','LOCALLY_CLOSED_EXTERNAL_PENDING')`, order.ID, now)
		}
	} else if current.CloseStatus != "PROVIDER_CLOSED" {
		if current.CloseStatus == "OPEN" {
			_, err = tx.Exec(ctx, `UPDATE payment_orders SET close_status='CLOSE_REQUESTED',close_requested_at=COALESCE(close_requested_at,$2),updated_at=$2 WHERE id=$1 AND close_status='OPEN'`, order.ID, now)
		}
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE payment_orders SET close_status='LOCALLY_CLOSED_EXTERNAL_PENDING',locally_closed_at=COALESCE(locally_closed_at,$2),updated_at=$2 WHERE id=$1 AND close_status='CLOSE_REQUESTED'`, order.ID, now)
		}
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE payment_orders SET status=CASE WHEN status='EXPIRED' THEN 'EXPIRED' ELSE 'CLOSED' END,close_status='PROVIDER_CLOSED',provider_closed_at=COALESCE(provider_closed_at,$2),payment_url=NULL,qr_data=NULL,reconciliation_next_at=NULL,reconciliation_lease_until=NULL,reconciliation_last_error=NULL,attention_summary='NORMAL',summary_version=summary_version+1,updated_at=$2 WHERE id=$1 AND close_status='LOCALLY_CLOSED_EXTERNAL_PENDING'`, order.ID, now)
		}
	}
	if err != nil {
		return PaymentOrder{}, err
	}
	updated, err := getPaymentOrderByID(ctx, tx, order.ID, false)
	if err != nil {
		return PaymentOrder{}, err
	}
	return updated, tx.Commit(ctx)
}

func (p *PaymentCoordinator) applyQueriedPayment(ctx context.Context, orderID uuid.UUID, provider ProviderOrder, reviewed bool) (PaymentOrder, error) {
	if provider.ChannelTransactionID == nil || provider.PaidAt == nil {
		return PaymentOrder{}, ErrConflict
	}
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
	order, _, mergeErr := p.mergeSuccessfulPaymentWithinTransaction(ctx, tx, order, "QUERY", providerEvidenceKey("QUERY", provider), nil, provider, reviewed, now)
	if mergeErr != nil && !errors.Is(mergeErr, ErrConflict) {
		return PaymentOrder{}, mergeErr
	}
	if err := tx.Commit(ctx); err != nil {
		return PaymentOrder{}, err
	}
	return order, mergeErr
}
