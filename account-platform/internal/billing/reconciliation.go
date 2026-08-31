package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type ReconciliationDifference struct {
	MerchantOrderNo  string
	PaymentOrderID   *uuid.UUID
	CaseType         string
	Severity         string
	Provider         *ProviderBillEntry
	Internal         *PaymentOrder
	InternalSnapshot json.RawMessage
	RefundSnapshot   json.RawMessage
	LedgerSnapshot   json.RawMessage
}

type ReconciliationRun struct {
	ID               uuid.UUID  `json:"reconciliation_run_id"`
	ProviderID       string     `json:"provider_id"`
	ProviderVersion  string     `json:"provider_version"`
	Channel          string     `json:"channel"`
	BusinessDate     string     `json:"business_date"`
	SourceMode       string     `json:"source_mode"`
	RequestedBy      *uuid.UUID `json:"requested_by,omitempty"`
	InputDigest      *string    `json:"input_digest,omitempty"`
	Status           string     `json:"status"`
	ProviderRowCount int        `json:"provider_row_count"`
	CaseCount        int        `json:"case_count"`
	StartedAt        time.Time  `json:"started_at"`
	CompletedAt      time.Time  `json:"completed_at"`
}

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

func (p *PaymentCoordinator) RepairReviewedOrder(ctx context.Context, orderID, actorID uuid.UUID, reason string) (PaymentOrder, error) {
	if !p.Capabilities().ManualRepairSupported || !p.Capabilities().OrderQuerySupported {
		return PaymentOrder{}, payment.ErrUnsupported
	}
	reason = strings.TrimSpace(reason)
	if actorID == uuid.Nil || reason == "" || len(reason) > 500 {
		return PaymentOrder{}, ErrConflict
	}
	order, err := getPaymentOrderByID(ctx, p.repository.pool, orderID, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentOrder{}, ErrNotFound
	}
	if err != nil {
		return PaymentOrder{}, err
	}
	if order.Status != "MANUAL_REVIEW" || order.FulfillmentStatus != "MANUAL_REVIEW" {
		return PaymentOrder{}, ErrConflict
	}
	provider, err := providerCall(ctx, p, "ORDER_QUERY", order.Channel, false, providerCallScope(order), func() (ProviderOrder, error) {
		return p.provider.Get(ctx, order.MerchantOrderNo)
	})
	if err != nil {
		return PaymentOrder{}, errors.Join(ErrPaymentProvider, err)
	}
	if provider.Status != "SUCCESS" || provider.ChannelTransactionID == nil || provider.PaidAt == nil || provider.MerchantOrderNo != order.MerchantOrderNo || provider.Channel != order.Channel || provider.AmountFen != order.AmountFen || provider.Currency != order.Currency {
		return PaymentOrder{}, ErrConflict
	}
	tx, err := p.repository.pool.Begin(ctx)
	if err != nil {
		return PaymentOrder{}, err
	}
	defer tx.Rollback(ctx)
	order, err = getPaymentOrderByID(ctx, tx, orderID, true)
	if err != nil || order.Status != "MANUAL_REVIEW" || order.FulfillmentStatus != "MANUAL_REVIEW" {
		if err != nil {
			return PaymentOrder{}, err
		}
		return PaymentOrder{}, ErrConflict
	}
	now := p.now()
	snapshot, _ := json.Marshal(map[string]any{
		"status": provider.Status, "provider_status": provider.ProviderStatus, "provider_trace_id": provider.ProviderTraceID,
		"provider_transaction_id": *provider.ChannelTransactionID, "paid_at": *provider.PaidAt,
	})
	var balanceBefore int64
	if err := tx.QueryRow(ctx, `SELECT posted_microcredits FROM wallets WHERE id=$1 FOR UPDATE`, order.WalletID).Scan(&balanceBefore); err != nil {
		return PaymentOrder{}, err
	}
	order, evidenceID, mergeErr := p.mergeSuccessfulPaymentWithinTransaction(ctx, tx, order, "MANUAL_REPAIR", "repair:"+order.ID.String()+":"+*provider.ChannelTransactionID, snapshot, provider, true, now)
	if mergeErr != nil {
		if !errors.Is(mergeErr, ErrConflict) {
			return PaymentOrder{}, mergeErr
		}
		if err := tx.Commit(ctx); err != nil {
			return PaymentOrder{}, err
		}
		return order, mergeErr
	}
	var transactionID, packageID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT transaction.id,package.id FROM payment_transactions transaction JOIN entitlement_packages package ON package.source_payment_transaction_id=transaction.id WHERE transaction.payment_order_id=$1 AND transaction.provider_transaction_id=$2`, order.ID, *provider.ChannelTransactionID).Scan(&transactionID, &packageID); err != nil {
		return PaymentOrder{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO payment_order_repairs(id,payment_order_id,payment_transaction_id,payment_evidence_id,actor_id,reason,provider_snapshot,entitlement_package_id,points_awarded,wallet_balance_before,wallet_balance_after,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT (payment_transaction_id) DO NOTHING`, uuid.Must(uuid.NewV7()), order.ID, transactionID, evidenceID, actorID, reason, snapshot, packageID, order.CreditMicrocredits, balanceBefore, balanceBefore+order.CreditMicrocredits, now); err != nil {
		return PaymentOrder{}, err
	}
	return order, tx.Commit(ctx)
}

func CompareReconciliation(provider []ProviderBillEntry, internal []PaymentOrder) []ReconciliationDifference {
	providerByMerchant := make(map[string]ProviderBillEntry, len(provider))
	transactionOwners := make(map[string]string, len(provider))
	differences := make([]ReconciliationDifference, 0)
	for i := range provider {
		item := provider[i]
		if owner, exists := transactionOwners[item.ChannelTransactionID]; exists && owner != item.MerchantOrderNo {
			copy := item
			differences = append(differences, ReconciliationDifference{MerchantOrderNo: item.MerchantOrderNo, CaseType: "DUPLICATE_TRANSACTION", Provider: &copy})
		} else {
			transactionOwners[item.ChannelTransactionID] = item.MerchantOrderNo
		}
		providerByMerchant[item.MerchantOrderNo] = item
	}
	internalByMerchant := make(map[string]PaymentOrder, len(internal))
	for i := range internal {
		item := internal[i]
		internalByMerchant[item.MerchantOrderNo] = item
		providerItem, exists := providerByMerchant[item.MerchantOrderNo]
		if !exists {
			copy := item
			differences = append(differences, ReconciliationDifference{MerchantOrderNo: item.MerchantOrderNo, PaymentOrderID: &copy.ID, CaseType: "INTERNAL_PAID_PROVIDER_MISSING", Internal: &copy})
			continue
		}
		if providerItem.AmountFen != item.AmountFen || providerItem.Currency != item.Currency {
			providerCopy, internalCopy := providerItem, item
			differences = append(differences, ReconciliationDifference{MerchantOrderNo: item.MerchantOrderNo, PaymentOrderID: &internalCopy.ID, CaseType: "AMOUNT_MISMATCH", Provider: &providerCopy, Internal: &internalCopy})
		}
	}
	for merchant, item := range providerByMerchant {
		if _, exists := internalByMerchant[merchant]; !exists {
			copy := item
			differences = append(differences, ReconciliationDifference{MerchantOrderNo: merchant, CaseType: "PROVIDER_PAID_INTERNAL_MISSING", Provider: &copy})
		}
	}
	return differences
}

func (p *PaymentCoordinator) RunReconciliation(ctx context.Context, channel string, businessDate time.Time) (ReconciliationRun, error) {
	return p.runProviderReconciliation(ctx, channel, businessDate, "MANUAL_API", nil)
}

func (p *PaymentCoordinator) RunAdminReconciliation(ctx context.Context, actorID uuid.UUID, channel string, businessDate time.Time) (ReconciliationRun, error) {
	if actorID == uuid.Nil {
		return ReconciliationRun{}, ErrConflict
	}
	return p.runProviderReconciliation(ctx, channel, businessDate, "MANUAL_API", &actorID)
}

func (p *PaymentCoordinator) RunScheduledReconciliation(ctx context.Context, channel string, businessDate time.Time) (ReconciliationRun, error) {
	return p.runProviderReconciliation(ctx, channel, businessDate, "SCHEDULED_API", nil)
}

func (p *PaymentCoordinator) runProviderReconciliation(ctx context.Context, channel string, businessDate time.Time, sourceMode string, actorID *uuid.UUID) (ReconciliationRun, error) {
	channel = strings.ToUpper(strings.TrimSpace(channel))
	if !p.Capabilities().BillSupported {
		return ReconciliationRun{}, payment.ErrUnsupported
	}
	if !validPaymentChannel(channel) || !p.Capabilities().SupportsChannel(channel) {
		return ReconciliationRun{}, ErrConflict
	}
	date := normalizedReconciliationBusinessDate(businessDate)
	providerRows, err := providerCall(ctx, p, "BILL_QUERY", channel, false, ProviderCallScope{}, func() ([]ProviderBillEntry, error) {
		return p.provider.Bill(ctx, channel, date)
	})
	if err != nil {
		return ReconciliationRun{}, errors.Join(ErrPaymentProvider, err)
	}
	return p.persistReconciliation(ctx, channel, date, providerRows, sourceMode, actorID)
}

func (p *PaymentCoordinator) ImportProviderBill(ctx context.Context, actorID uuid.UUID, channel string, businessDate time.Time, providerRows []ProviderBillEntry) (ReconciliationRun, error) {
	channel = strings.ToUpper(strings.TrimSpace(channel))
	if !p.Capabilities().BillFileImportSupported || actorID == uuid.Nil || !validPaymentChannel(channel) || !p.Capabilities().SupportsChannel(channel) || len(providerRows) > 100000 {
		return ReconciliationRun{}, payment.ErrUnsupported
	}
	date := normalizedReconciliationBusinessDate(businessDate)
	start, end := reconciliationBusinessDateRange(date)
	for index := range providerRows {
		row := &providerRows[index]
		if strings.TrimSpace(row.Channel) == "" {
			row.Channel = channel
		}
		entryType := normalizedBillEntryType(*row)
		if strings.TrimSpace(row.MerchantOrderNo) == "" || billReference(*row) == "" || row.AmountFen <= 0 || row.Currency == "" || !strings.EqualFold(row.Channel, channel) || row.PaidAt.Before(start) || !row.PaidAt.Before(end) || (entryType != "PAYMENT" && entryType != "REFUND" && entryType != "CHARGEBACK") {
			return ReconciliationRun{}, ErrConflict
		}
	}
	return p.persistReconciliation(ctx, channel, date, providerRows, "FILE_IMPORT", &actorID)
}

func (p *PaymentCoordinator) persistReconciliation(ctx context.Context, channel string, date time.Time, providerRows []ProviderBillEntry, sourceMode string, importActorID *uuid.UUID) (ReconciliationRun, error) {
	var importDigest string
	isFileImport := sourceMode == "FILE_IMPORT"
	if isFileImport {
		encoded, marshalErr := json.Marshal(providerRows)
		if marshalErr != nil {
			return ReconciliationRun{}, marshalErr
		}
		digest := sha256.Sum256(encoded)
		importDigest = hex.EncodeToString(digest[:])
	}
	tx, err := p.repository.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ReconciliationRun{}, err
	}
	defer tx.Rollback(ctx)
	lockKey := p.Capabilities().ProviderID + ":" + p.Capabilities().Version + ":" + channel + ":" + date.Format("2006-01-02") + ":" + sourceMode + ":" + importDigest
	if err := lockTransactionKey(ctx, tx, "payment-reconciliation", lockKey); err != nil {
		return ReconciliationRun{}, err
	}
	if isFileImport {
		var existing ReconciliationRun
		err := tx.QueryRow(ctx, `SELECT run.id,run.provider_id,run.provider_version,run.channel,run.business_date::text,run.source_mode,run.requested_by,run.input_digest,run.status,run.provider_row_count,run.case_count,run.started_at,run.completed_at FROM provider_bill_imports import JOIN reconciliation_runs run ON run.id=import.reconciliation_run_id WHERE import.provider_id=$1 AND import.provider_version=$2 AND import.channel=$3 AND import.business_date=$4 AND import.payload_digest=$5`, p.Capabilities().ProviderID, p.Capabilities().Version, channel, date, importDigest).Scan(&existing.ID, &existing.ProviderID, &existing.ProviderVersion, &existing.Channel, &existing.BusinessDate, &existing.SourceMode, &existing.RequestedBy, &existing.InputDigest, &existing.Status, &existing.ProviderRowCount, &existing.CaseCount, &existing.StartedAt, &existing.CompletedAt)
		if err == nil {
			return existing, tx.Commit(ctx)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return ReconciliationRun{}, err
		}
	}
	if sourceMode == "SCHEDULED_API" {
		var existing ReconciliationRun
		err := tx.QueryRow(ctx, `SELECT id,provider_id,provider_version,channel,business_date::text,source_mode,requested_by,input_digest,status,provider_row_count,case_count,started_at,completed_at FROM reconciliation_runs WHERE provider_id=$1 AND provider_version=$2 AND channel=$3 AND business_date=$4 AND source_mode='SCHEDULED_API'`, p.Capabilities().ProviderID, p.Capabilities().Version, channel, date).Scan(&existing.ID, &existing.ProviderID, &existing.ProviderVersion, &existing.Channel, &existing.BusinessDate, &existing.SourceMode, &existing.RequestedBy, &existing.InputDigest, &existing.Status, &existing.ProviderRowCount, &existing.CaseCount, &existing.StartedAt, &existing.CompletedAt)
		if err == nil {
			return existing, tx.Commit(ctx)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return ReconciliationRun{}, err
		}
	}
	now := p.now()
	for _, row := range providerRows {
		if err := p.mergeProviderBillEvidenceWithinTransaction(ctx, tx, row, now); err != nil {
			return ReconciliationRun{}, err
		}
	}
	start, end := reconciliationBusinessDateRange(date)
	internalFacts, err := p.listFourWayReconciliationFacts(ctx, tx, channel, start, end)
	if err != nil {
		return ReconciliationRun{}, err
	}
	differences := CompareFourWayReconciliation(providerRows, internalFacts)
	run := ReconciliationRun{ID: uuid.Must(uuid.NewV7()), ProviderID: p.Capabilities().ProviderID, ProviderVersion: p.Capabilities().Version, Channel: channel, BusinessDate: date.Format("2006-01-02"), SourceMode: sourceMode, RequestedBy: importActorID, Status: "COMPLETED", ProviderRowCount: len(providerRows), CaseCount: len(differences), StartedAt: now, CompletedAt: now}
	if importDigest != "" {
		run.InputDigest = &importDigest
	}
	_, err = tx.Exec(ctx, `INSERT INTO reconciliation_runs(id,provider_id,provider_version,channel,business_date,source_mode,requested_by,input_digest,status,provider_row_count,case_count,started_at,completed_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'COMPLETED',$9,$10,$11,$11)`, run.ID, run.ProviderID, run.ProviderVersion, channel, date, sourceMode, importActorID, run.InputDigest, len(providerRows), len(differences), now)
	if err != nil {
		return ReconciliationRun{}, err
	}
	for index, row := range providerRows {
		rawSnapshot, marshalErr := json.Marshal(row)
		if marshalErr != nil {
			return ReconciliationRun{}, marshalErr
		}
		_, err = tx.Exec(ctx, `INSERT INTO reconciliation_provider_rows(reconciliation_run_id,row_number,entry_type,provider_reference_id,channel_transaction_id,merchant_order_no,channel,amount_fen,currency,status,paid_at,raw_snapshot,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, run.ID, index+1, normalizedBillEntryType(row), billReference(row), row.ChannelTransactionID, row.MerchantOrderNo, channel, row.AmountFen, row.Currency, row.Status, row.PaidAt, rawSnapshot, now)
		if err != nil {
			return ReconciliationRun{}, err
		}
	}
	if isFileImport {
		importID := uuid.Must(uuid.NewV7())
		_, err = tx.Exec(ctx, `INSERT INTO provider_bill_imports(id,provider_id,provider_version,channel,business_date,payload_digest,row_count,actor_id,reconciliation_run_id,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, importID, p.Capabilities().ProviderID, p.Capabilities().Version, channel, date, importDigest, len(providerRows), *importActorID, run.ID, now)
		if err != nil {
			return ReconciliationRun{}, err
		}
		for index, row := range providerRows {
			_, err = tx.Exec(ctx, `INSERT INTO provider_bill_import_rows(import_id,row_number,merchant_order_no,provider_transaction_id,amount_fen,currency,status,paid_at,entry_type,provider_reference_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(import_id,row_number) DO NOTHING`, importID, index+1, row.MerchantOrderNo, row.ChannelTransactionID, row.AmountFen, row.Currency, row.Status, row.PaidAt, normalizedBillEntryType(row), billReference(row))
			if err != nil {
				return ReconciliationRun{}, err
			}
		}
	}
	for _, difference := range differences {
		providerJSON := json.RawMessage(`{}`)
		if difference.Provider != nil {
			providerJSON, _ = json.Marshal(difference.Provider)
		}
		internalJSON := difference.InternalSnapshot
		if len(internalJSON) == 0 {
			internalJSON = json.RawMessage(`{}`)
			if difference.Internal != nil {
				internalJSON, _ = json.Marshal(difference.Internal)
			}
		}
		refundJSON := difference.RefundSnapshot
		if len(refundJSON) == 0 {
			refundJSON = json.RawMessage(`{}`)
		}
		ledgerJSON := difference.LedgerSnapshot
		if len(ledgerJSON) == 0 {
			ledgerJSON = json.RawMessage(`{}`)
		}
		caseID := uuid.Must(uuid.NewV7())
		claim, response, mitigation, resolution := paymentAnomalyDeadlines(now, difference.Severity)
		_, err = tx.Exec(ctx, `INSERT INTO manual_review_cases(id,domain,case_type,resource_type,resource_id,severity,status,created_at,claim_due_at,first_response_due_at,mitigation_due_at,resolution_due_at,updated_at) VALUES($1,'BILLING','RECONCILIATION_DIFFERENCE','RECONCILIATION_CASE',$1,$2,'OPEN',$3,$4,$5,$6,$7,$3)`, caseID, difference.Severity, now, claim, response, mitigation, resolution)
		if err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO reconciliation_cases(id,reconciliation_run_id,payment_order_id,merchant_order_no,case_type,status,severity,provider_snapshot,internal_snapshot,refund_snapshot,ledger_snapshot,manual_review_case_id,created_at,updated_at) VALUES($1,$2,$3,$4,$5,'OPEN',$6,$7,$8,$9,$10,$1,$11,$11)`, caseID, run.ID, difference.PaymentOrderID, difference.MerchantOrderNo, difference.CaseType, difference.Severity, providerJSON, internalJSON, refundJSON, ledgerJSON, now)
		}
		if err == nil {
			err = appendManualReviewEvent(ctx, tx, caseID, "CREATED", importActorID, map[string]any{"case_type": difference.CaseType, "reconciliation_run_id": run.ID}, now)
		}
		if err != nil {
			return ReconciliationRun{}, err
		}
	}
	return run, tx.Commit(ctx)
}

func (p *PaymentCoordinator) mergeProviderBillEvidenceWithinTransaction(ctx context.Context, tx pgx.Tx, row ProviderBillEntry, receivedAt time.Time) error {
	payload, err := json.Marshal(row)
	if err != nil {
		return err
	}
	entryType := normalizedBillEntryType(row)
	reference := billReference(row)
	evidenceKey := fmt.Sprintf("%s|%s|%s|%s", entryType, reference, strings.TrimSpace(row.MerchantOrderNo), row.PaidAt.UTC().Format(time.RFC3339Nano))
	order, orderErr := getPaymentOrderForUpdate(ctx, tx, strings.TrimSpace(row.MerchantOrderNo))
	matchedOrder := orderErr == nil && order.ProviderID == p.Capabilities().ProviderID && order.ProviderVersion == p.Capabilities().Version
	if orderErr != nil && !errors.Is(orderErr, pgx.ErrNoRows) {
		return orderErr
	}
	status := strings.ToUpper(strings.TrimSpace(row.Status))
	isSuccessfulPayment := entryType == "PAYMENT" && (status == "SUCCESS" || status == "PAID" || status == "COMPLETED") && strings.TrimSpace(row.ChannelTransactionID) != ""
	if matchedOrder && isSuccessfulPayment {
		transactionID := strings.TrimSpace(row.ChannelTransactionID)
		paidAt := row.PaidAt.UTC()
		providerOrder := ProviderOrder{
			MerchantOrderNo:      row.MerchantOrderNo,
			Status:               "SUCCESS",
			Channel:              row.Channel,
			AmountFen:            row.AmountFen,
			Currency:             row.Currency,
			ChannelTransactionID: &transactionID,
			PaidAt:               &paidAt,
			ProviderStatus:       row.Status,
		}
		_, _, mergeErr := p.mergeSuccessfulPaymentWithinTransaction(ctx, tx, order, "BILL", evidenceKey, payload, providerOrder, false, receivedAt)
		if mergeErr == nil || errors.Is(mergeErr, ErrConflict) {
			return nil
		}
		return mergeErr
	}

	var orderID *uuid.UUID
	var transactionID *uuid.UUID
	if matchedOrder {
		orderID = &order.ID
		var found uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM payment_transactions WHERE payment_order_id=$1 AND provider_transaction_id=$2`, order.ID, strings.TrimSpace(row.ChannelTransactionID)).Scan(&found); err == nil {
			transactionID = &found
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	digest := sha256.Sum256(payload)
	evidenceID := uuid.Must(uuid.NewV7())
	_, err = tx.Exec(ctx, `INSERT INTO payment_evidence(
id,payment_order_id,payment_transaction_id,evidence_type,provider_id,provider_version,evidence_key,
merchant_order_no,provider_transaction_id,amount_fen,currency,provider_status,provider_event_at,
received_at,payload_digest,payload_snapshot,merge_result)
VALUES($1,$2,$3,'BILL',$4,$5,$6,$7,NULLIF($8,''),$9,$10,NULLIF($11,''),$12,$13,$14,$15,'REVIEW_REQUIRED')
ON CONFLICT(provider_id,provider_version,evidence_type,evidence_key,payload_digest) DO NOTHING`,
		evidenceID, orderID, transactionID, p.Capabilities().ProviderID, p.Capabilities().Version, evidenceKey,
		row.MerchantOrderNo, strings.TrimSpace(row.ChannelTransactionID), row.AmountFen, row.Currency, row.Status,
		row.PaidAt.UTC(), receivedAt, hex.EncodeToString(digest[:]), json.RawMessage(payload))
	return err
}

type ReconciliationInternalFact struct {
	EntryType           string
	ProviderReferenceID string
	MerchantOrderNo     string
	PaymentOrderID      uuid.UUID
	AmountFen           int64
	Currency            string
	InternalSnapshot    json.RawMessage
	RefundSnapshot      json.RawMessage
	LedgerSnapshot      json.RawMessage
	LedgerPresent       bool
}

func normalizedBillEntryType(item ProviderBillEntry) string {
	entryType := strings.ToUpper(strings.TrimSpace(item.EntryType))
	if entryType != "" {
		return entryType
	}
	status := strings.ToUpper(strings.TrimSpace(item.Status))
	if strings.Contains(status, "CHARGEBACK") {
		return "CHARGEBACK"
	}
	if strings.Contains(status, "REFUND") {
		return "REFUND"
	}
	return "PAYMENT"
}

func billReference(item ProviderBillEntry) string {
	if value := strings.TrimSpace(item.ProviderReferenceID); value != "" {
		return value
	}
	return strings.TrimSpace(item.ChannelTransactionID)
}

func CompareFourWayReconciliation(provider []ProviderBillEntry, internal []ReconciliationInternalFact) []ReconciliationDifference {
	providerByKey := map[string]ProviderBillEntry{}
	differences := []ReconciliationDifference{}
	for index := range provider {
		item := provider[index]
		entryType, reference := normalizedBillEntryType(item), billReference(item)
		if entryType != "PAYMENT" && entryType != "REFUND" && entryType != "CHARGEBACK" {
			copy := item
			differences = append(differences, ReconciliationDifference{MerchantOrderNo: item.MerchantOrderNo, CaseType: "UNSUPPORTED_PROVIDER_ENTRY", Severity: "P0", Provider: &copy})
			continue
		}
		key := entryType + "|" + reference
		if _, exists := providerByKey[key]; exists {
			copy := item
			differences = append(differences, ReconciliationDifference{MerchantOrderNo: item.MerchantOrderNo, CaseType: "DUPLICATE_TRANSACTION", Severity: "P0", Provider: &copy})
			continue
		}
		providerByKey[key] = item
	}
	internalByKey := map[string]ReconciliationInternalFact{}
	for index := range internal {
		fact := internal[index]
		key := fact.EntryType + "|" + fact.ProviderReferenceID
		if _, exists := internalByKey[key]; exists {
			differences = append(differences, differenceFromFact(fact, "DUPLICATE_TRANSACTION", "P0", nil))
			continue
		}
		internalByKey[key] = fact
		providerItem, exists := providerByKey[key]
		caseTypePrefix := map[string]string{"PAYMENT": "PAID", "REFUND": "REFUND", "CHARGEBACK": "CHARGEBACK"}[fact.EntryType]
		if !exists {
			caseType := "INTERNAL_" + caseTypePrefix + "_PROVIDER_MISSING"
			differences = append(differences, differenceFromFact(fact, caseType, "P1", nil))
		} else if providerItem.AmountFen != fact.AmountFen || !strings.EqualFold(providerItem.Currency, fact.Currency) {
			caseType := "AMOUNT_MISMATCH"
			if fact.EntryType == "REFUND" {
				caseType = "REFUND_AMOUNT_MISMATCH"
			}
			copy := providerItem
			differences = append(differences, differenceFromFact(fact, caseType, "P0", &copy))
		}
		if !fact.LedgerPresent {
			caseType := map[string]string{"PAYMENT": "PAYMENT_LEDGER_MISSING", "REFUND": "REFUND_LEDGER_MISSING", "CHARGEBACK": "CHARGEBACK_LEDGER_MISSING"}[fact.EntryType]
			differences = append(differences, differenceFromFact(fact, caseType, "P0", nil))
		}
	}
	for key, item := range providerByKey {
		if _, exists := internalByKey[key]; exists {
			continue
		}
		entryType := normalizedBillEntryType(item)
		caseType := map[string]string{"PAYMENT": "PROVIDER_PAID_INTERNAL_MISSING", "REFUND": "PROVIDER_REFUND_INTERNAL_MISSING", "CHARGEBACK": "PROVIDER_CHARGEBACK_INTERNAL_MISSING"}[entryType]
		copy := item
		differences = append(differences, ReconciliationDifference{MerchantOrderNo: item.MerchantOrderNo, CaseType: caseType, Severity: "P0", Provider: &copy})
	}
	return differences
}

func differenceFromFact(fact ReconciliationInternalFact, caseType, severity string, provider *ProviderBillEntry) ReconciliationDifference {
	orderID := fact.PaymentOrderID
	return ReconciliationDifference{MerchantOrderNo: fact.MerchantOrderNo, PaymentOrderID: &orderID, CaseType: caseType, Severity: severity, Provider: provider, InternalSnapshot: fact.InternalSnapshot, RefundSnapshot: fact.RefundSnapshot, LedgerSnapshot: fact.LedgerSnapshot}
}

type reconciliationQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func (p *PaymentCoordinator) listFourWayReconciliationFacts(ctx context.Context, querier reconciliationQuerier, channel string, start, end time.Time) ([]ReconciliationInternalFact, error) {
	rows, err := querier.Query(ctx, `
SELECT 'PAYMENT',transaction.provider_transaction_id,payment.merchant_order_no,payment.id,transaction.amount_fen,transaction.currency,
       jsonb_build_object('payment_order',to_jsonb(payment),'payment_transaction',to_jsonb(transaction)),NULL,
       COALESCE((SELECT jsonb_agg(to_jsonb(entry) ORDER BY entry.created_at,entry.id) FROM journal_entries entry WHERE entry.business_reference IN ('payment-transaction:'||transaction.id::text,'repair-payment-transaction:'||transaction.id::text)),'[]'::jsonb)
FROM payment_transactions transaction JOIN payment_orders payment ON payment.id=transaction.payment_order_id
WHERE transaction.channel=$1 AND transaction.provider_paid_at>=$2 AND transaction.provider_paid_at<$3 AND transaction.status IN ('PAID','LATE_PAID')
UNION ALL
SELECT 'REFUND',refund.channel_refund_id,payment.merchant_order_no,payment.id,refund.amount_fen,payment.currency,
       jsonb_build_object('payment_order_id',payment.id,'payment_transaction_id',refund.payment_transaction_id),to_jsonb(refund),
       COALESCE((SELECT jsonb_agg(to_jsonb(entry) ORDER BY entry.created_at,entry.id) FROM journal_entries entry WHERE entry.business_reference='refund:'||refund.id::text),'[]'::jsonb)
FROM refunds refund JOIN payment_orders payment ON payment.id=refund.payment_order_id
WHERE payment.channel=$1 AND refund.updated_at>=$2 AND refund.updated_at<$3 AND refund.status='SUCCEEDED' AND refund.channel_refund_id IS NOT NULL
UNION ALL
SELECT 'REFUND',forced.provider_refund_id,payment.merchant_order_no,payment.id,forced.amount_fen,payment.currency,
       jsonb_build_object('payment_order_id',payment.id,'payment_transaction_id',forced.payment_transaction_id),to_jsonb(forced),
       COALESCE((SELECT jsonb_agg(to_jsonb(entry) ORDER BY entry.created_at,entry.id) FROM journal_entries entry WHERE entry.business_reference='forced-refund:'||forced.id::text),'[]'::jsonb)
FROM forced_refund_cases forced JOIN payment_orders payment ON payment.id=forced.payment_order_id
WHERE payment.channel=$1 AND forced.resolved_at>=$2 AND forced.resolved_at<$3 AND forced.resolution_code='CONFIRM_FORCED_REFUND'
UNION ALL
SELECT 'CHARGEBACK',chargeback.provider_chargeback_id,payment.merchant_order_no,payment.id,transaction.amount_fen,transaction.currency,
       jsonb_build_object('payment_order_id',payment.id,'payment_transaction_id',chargeback.payment_transaction_id),to_jsonb(chargeback),
       COALESCE((SELECT jsonb_agg(to_jsonb(entry) ORDER BY entry.created_at,entry.id) FROM journal_entries entry WHERE entry.business_reference='payment-transaction:'||chargeback.payment_transaction_id::text||':chargeback:'||chargeback.provider_chargeback_id),'[]'::jsonb)
FROM chargeback_cases chargeback JOIN payment_orders payment ON payment.id=chargeback.payment_order_id JOIN payment_transactions transaction ON transaction.id=chargeback.payment_transaction_id
WHERE payment.channel=$1 AND chargeback.resolved_at>=$2 AND chargeback.resolved_at<$3 AND chargeback.status='CONFIRMED'
ORDER BY 1,2`, channel, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	facts := []ReconciliationInternalFact{}
	for rows.Next() {
		var fact ReconciliationInternalFact
		if err := rows.Scan(&fact.EntryType, &fact.ProviderReferenceID, &fact.MerchantOrderNo, &fact.PaymentOrderID, &fact.AmountFen, &fact.Currency, &fact.InternalSnapshot, &fact.RefundSnapshot, &fact.LedgerSnapshot); err != nil {
			return nil, err
		}
		fact.LedgerPresent = len(fact.LedgerSnapshot) > 0 && string(fact.LedgerSnapshot) != "[]"
		facts = append(facts, fact)
	}
	return facts, rows.Err()
}

func (p *PaymentCoordinator) listPaidOrders(ctx context.Context, channel string, start, end time.Time) ([]PaymentOrder, error) {
	rows, err := p.repository.pool.Query(ctx, `SELECT id,merchant_order_no,requester_id,beneficiary_organization_id,wallet_id,product_id,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,COALESCE(payment_url,''),primary_provider_transaction_id,expires_at,paid_at,provider_id,provider_version FROM payment_orders WHERE channel=$1 AND paid_at >= $2 AND paid_at < $3 AND fulfillment_status='CREDITED' ORDER BY paid_at,merchant_order_no`, channel, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]PaymentOrder, 0)
	for rows.Next() {
		var item PaymentOrder
		if err := rows.Scan(&item.ID, &item.MerchantOrderNo, &item.RequesterID, &item.BeneficiaryOrganizationID, &item.WalletID, &item.ProductID, &item.Channel, &item.AmountFen, &item.Currency, &item.CreditMicrocredits, &item.Status, &item.FulfillmentStatus, &item.PaymentURL, &item.PrimaryProviderTransactionID, &item.ExpiresAt, &item.PaidAt, &item.ProviderID, &item.ProviderVersion); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

type scheduledPaymentReconciler interface {
	Capabilities() PaymentCapabilities
	RunScheduledReconciliation(context.Context, string, time.Time) (ReconciliationRun, error)
}

type DailyPaymentReconciliationWorker struct {
	payments scheduledPaymentReconciler
	logger   *slog.Logger
	now      func() time.Time
	interval time.Duration
}

func NewDailyPaymentReconciliationWorker(payments scheduledPaymentReconciler) *DailyPaymentReconciliationWorker {
	return &DailyPaymentReconciliationWorker{payments: payments, logger: slog.Default(), now: time.Now, interval: time.Hour}
}

func reconciliationBusinessDate(now time.Time) time.Time {
	location := time.FixedZone("Asia/Shanghai", 8*60*60)
	local := now.In(location).AddDate(0, 0, -1)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
}

func normalizedReconciliationBusinessDate(value time.Time) time.Time {
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
}

func reconciliationBusinessDateRange(date time.Time) (time.Time, time.Time) {
	location := time.FixedZone("Asia/Shanghai", 8*60*60)
	start := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, location)
	return start, start.AddDate(0, 0, 1)
}

func (w *DailyPaymentReconciliationWorker) Run(ctx context.Context) {
	w.runOnce(ctx)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.runOnce(ctx)
		}
	}
}

func (w *DailyPaymentReconciliationWorker) runOnce(ctx context.Context) {
	if w.payments == nil || !w.payments.Capabilities().BillSupported {
		return
	}
	date := reconciliationBusinessDate(w.now())
	for _, channel := range w.payments.Capabilities().Channels {
		if _, err := w.payments.RunScheduledReconciliation(ctx, channel, date); err != nil {
			w.logger.Error("scheduled payment reconciliation failed", "channel", channel, "business_date", date.Format("2006-01-02"), "error", err)
		}
	}
}
