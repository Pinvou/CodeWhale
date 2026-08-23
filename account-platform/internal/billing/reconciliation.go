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
	MerchantOrderNo string
	PaymentOrderID  *uuid.UUID
	CaseType        string
	Provider        *ProviderBillEntry
	Internal        *PaymentOrder
}

type ReconciliationRun struct {
	ID               uuid.UUID `json:"reconciliation_run_id"`
	Channel          string    `json:"channel"`
	BusinessDate     string    `json:"business_date"`
	Status           string    `json:"status"`
	ProviderRowCount int       `json:"provider_row_count"`
	CaseCount        int       `json:"case_count"`
	StartedAt        time.Time `json:"started_at"`
	CompletedAt      time.Time `json:"completed_at"`
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
		if syncErr == nil && (order.Status == "CREATED" || order.Status == "PENDING") {
			if !item.LateReviewBoundaryAt.After(r.now()) {
				if _, err := r.store.EscalateLatePaymentReview(ctx, item.OrderID, "PENDING"); err != nil {
					r.logger.Error("escalate late payment review failed", "order_id", item.OrderID, "error", err)
				}
				continue
			}
			// Providers without notifications converge through server-side polling,
			// including after the client closes the QR page. A non-terminal state is
			// expected here and is rescheduled rather than treated as an error.
			nextAt := r.now().Add(r.interval)
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
    WHERE status IN ('CREATED','PENDING')
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
WHERE id=$1 AND status IN ('CREATED','PENDING')`, orderID, nextAt.UTC(), truncatePaymentReconciliationError(lastError), r.now())
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
	providerOrder, err := p.provider.Get(ctx, order.MerchantOrderNo)
	if err != nil {
		return PaymentOrder{}, errors.Join(ErrPaymentProvider, err)
	}
	if _, err := p.repository.pool.Exec(ctx, `UPDATE payment_orders SET last_successful_query_at=$2,last_authoritative_unpaid_at=CASE WHEN $3 AND $4 IN ('CLOSED','FAILED') THEN $2 ELSE last_authoritative_unpaid_at END,updated_at=$2 WHERE id=$1`, order.ID, p.now(), p.Capabilities().AuthoritativeUnpaidTerminal, providerOrder.Status); err != nil {
		return PaymentOrder{}, err
	}
	if _, err := p.repository.pool.Exec(ctx, `UPDATE payment_orders SET payment_url=NULLIF($2,''),qr_data=NULLIF($3,''),provider_status=NULLIF($4,''),provider_trace_id=NULLIF($5,''),updated_at=$6 WHERE id=$1`, order.ID, providerOrder.PaymentURL, providerOrder.QRData, providerOrder.ProviderStatus, providerOrder.ProviderTraceID, p.now()); err != nil {
		return PaymentOrder{}, err
	}
	order.PaymentURL, order.QRData = providerOrder.PaymentURL, providerOrder.QRData
	if providerOrder.Status == "SUCCESS" {
		return p.applyQueriedPayment(ctx, order.ID, providerOrder, reviewed)
	}
	if err := p.recordProviderObservation(ctx, order.ID, "QUERY", providerOrder); err != nil {
		return PaymentOrder{}, err
	}
	if providerOrder.Status == "PARTIALLY_REFUNDED" || providerOrder.Status == "REFUNDED" {
		return p.applyProviderReviewStatus(ctx, order)
	}
	if providerOrder.Status == "CLOSED" || providerOrder.Status == "FAILED" {
		if !p.Capabilities().AuthoritativeUnpaidTerminal {
			return order, nil
		}
		return p.applyProviderTerminalStatus(ctx, order, providerOrder.Status)
	}
	if (order.Status == "CREATED" || order.Status == "PENDING") && !order.ExpiresAt.After(p.now()) && providerOrder.Status == "PENDING" {
		if !p.Capabilities().CloseSupported {
			return order, nil
		}
		closed, closeErr := p.provider.Close(ctx, order.MerchantOrderNo)
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
			return p.applyProviderReviewStatus(ctx, order)
		case "CLOSED", "FAILED":
			if err := p.recordProviderObservation(ctx, order.ID, "QUERY", closed); err != nil {
				return PaymentOrder{}, err
			}
			if !p.Capabilities().AuthoritativeUnpaidTerminal {
				return order, nil
			}
			return p.applyProviderTerminalStatus(ctx, order, closed.Status)
		}
	}
	return order, nil
}

func (p *PaymentCoordinator) applyProviderReviewStatus(ctx context.Context, order PaymentOrder) (PaymentOrder, error) {
	_, err := p.repository.pool.Exec(ctx, `UPDATE payment_orders
SET status=CASE WHEN status IN ('CREATED','PENDING') AND fulfillment_status='PENDING' THEN 'MANUAL_REVIEW' ELSE status END,
    fulfillment_status=CASE WHEN status IN ('CREATED','PENDING') AND fulfillment_status='PENDING' THEN 'MANUAL_REVIEW' ELSE fulfillment_status END,
    attention_summary='REVIEW_REQUIRED',summary_version=summary_version+1,reconciliation_next_at=NULL,reconciliation_lease_until=NULL,reconciliation_last_error=NULL,updated_at=$2
WHERE id=$1`, order.ID, p.now())
	if err != nil {
		return PaymentOrder{}, err
	}
	return getPaymentOrderByID(ctx, p.repository.pool, order.ID, false)
}

func (p *PaymentCoordinator) applyProviderTerminalStatus(ctx context.Context, order PaymentOrder, status string) (PaymentOrder, error) {
	if status != "CLOSED" && status != "FAILED" {
		return PaymentOrder{}, fmt.Errorf("unsupported provider terminal status %q", status)
	}
	_, err := p.repository.pool.Exec(ctx, `UPDATE payment_orders
SET status=$2,close_status=$2,reconciliation_next_at=NULL,reconciliation_lease_until=NULL,reconciliation_last_error=NULL,updated_at=$3
WHERE id=$1 AND status IN ('CREATED','PENDING')`, order.ID, status, p.now())
	if err != nil {
		return PaymentOrder{}, err
	}
	return getPaymentOrderByID(ctx, p.repository.pool, order.ID, false)
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
	provider, err := p.provider.Get(ctx, order.MerchantOrderNo)
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
	channel = strings.ToUpper(strings.TrimSpace(channel))
	if !p.Capabilities().BillSupported {
		return ReconciliationRun{}, payment.ErrUnsupported
	}
	if !validPaymentChannel(channel) || !p.Capabilities().SupportsChannel(channel) {
		return ReconciliationRun{}, ErrConflict
	}
	date := businessDate.UTC()
	providerRows, err := p.provider.Bill(ctx, channel, date)
	if err != nil {
		return ReconciliationRun{}, errors.Join(ErrPaymentProvider, err)
	}
	return p.persistReconciliation(ctx, channel, date, providerRows, nil)
}

func (p *PaymentCoordinator) ImportProviderBill(ctx context.Context, actorID uuid.UUID, channel string, businessDate time.Time, providerRows []ProviderBillEntry) (ReconciliationRun, error) {
	channel = strings.ToUpper(strings.TrimSpace(channel))
	if !p.Capabilities().BillFileImportSupported || actorID == uuid.Nil || !validPaymentChannel(channel) || !p.Capabilities().SupportsChannel(channel) || len(providerRows) > 100000 {
		return ReconciliationRun{}, payment.ErrUnsupported
	}
	date := businessDate.UTC()
	for _, row := range providerRows {
		if strings.TrimSpace(row.MerchantOrderNo) == "" || strings.TrimSpace(row.ChannelTransactionID) == "" || row.AmountFen <= 0 || row.Currency == "" || !strings.EqualFold(row.Channel, channel) || row.PaidAt.Before(date) || !row.PaidAt.Before(date.AddDate(0, 0, 1)) {
			return ReconciliationRun{}, ErrConflict
		}
	}
	return p.persistReconciliation(ctx, channel, date, providerRows, &actorID)
}

func (p *PaymentCoordinator) persistReconciliation(ctx context.Context, channel string, date time.Time, providerRows []ProviderBillEntry, importActorID *uuid.UUID) (ReconciliationRun, error) {
	var importDigest string
	if importActorID != nil {
		encoded, marshalErr := json.Marshal(providerRows)
		if marshalErr != nil {
			return ReconciliationRun{}, marshalErr
		}
		digest := sha256.Sum256(encoded)
		importDigest = hex.EncodeToString(digest[:])
		var existing ReconciliationRun
		err := p.repository.pool.QueryRow(ctx, `SELECT run.id,run.channel,run.business_date::text,run.status,run.provider_row_count,run.case_count,run.started_at,run.completed_at FROM provider_bill_imports import JOIN reconciliation_runs run ON run.id=import.reconciliation_run_id WHERE import.provider_id=$1 AND import.provider_version=$2 AND import.channel=$3 AND import.business_date=$4 AND import.payload_digest=$5`, p.Capabilities().ProviderID, p.Capabilities().Version, channel, date, importDigest).Scan(&existing.ID, &existing.Channel, &existing.BusinessDate, &existing.Status, &existing.ProviderRowCount, &existing.CaseCount, &existing.StartedAt, &existing.CompletedAt)
		if err == nil {
			return existing, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return ReconciliationRun{}, err
		}
	}
	start, end := date, date.AddDate(0, 0, 1)
	internalRows, err := p.listPaidOrders(ctx, channel, start, end)
	if err != nil {
		return ReconciliationRun{}, err
	}
	differences := CompareReconciliation(providerRows, internalRows)
	now := p.now()
	run := ReconciliationRun{ID: uuid.Must(uuid.NewV7()), Channel: channel, BusinessDate: date.Format("2006-01-02"), Status: "COMPLETED", ProviderRowCount: len(providerRows), CaseCount: len(differences), StartedAt: now, CompletedAt: now}
	tx, err := p.repository.pool.Begin(ctx)
	if err != nil {
		return ReconciliationRun{}, err
	}
	defer tx.Rollback(ctx)
	err = tx.QueryRow(ctx, `INSERT INTO reconciliation_runs(id,channel,business_date,status,provider_row_count,case_count,started_at,completed_at) VALUES($1,$2,$3,'COMPLETED',$4,$5,$6,$6) ON CONFLICT(channel,business_date) DO UPDATE SET status='COMPLETED',provider_row_count=EXCLUDED.provider_row_count,case_count=EXCLUDED.case_count,started_at=EXCLUDED.started_at,completed_at=EXCLUDED.completed_at,error_message=NULL RETURNING id`, run.ID, channel, date, len(providerRows), len(differences), now).Scan(&run.ID)
	if err != nil {
		return ReconciliationRun{}, err
	}
	if importActorID != nil {
		importID := uuid.Must(uuid.NewV7())
		_, err = tx.Exec(ctx, `INSERT INTO provider_bill_imports(id,provider_id,provider_version,channel,business_date,payload_digest,row_count,actor_id,reconciliation_run_id,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, importID, p.Capabilities().ProviderID, p.Capabilities().Version, channel, date, importDigest, len(providerRows), *importActorID, run.ID, now)
		if err != nil {
			return ReconciliationRun{}, err
		}
		for index, row := range providerRows {
			_, err = tx.Exec(ctx, `INSERT INTO provider_bill_import_rows(import_id,row_number,merchant_order_no,provider_transaction_id,amount_fen,currency,status,paid_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(import_id,row_number) DO NOTHING`, importID, index+1, row.MerchantOrderNo, row.ChannelTransactionID, row.AmountFen, row.Currency, row.Status, row.PaidAt)
			if err != nil {
				return ReconciliationRun{}, err
			}
		}
	}
	if _, err = tx.Exec(ctx, `DELETE FROM reconciliation_cases WHERE reconciliation_run_id=$1`, run.ID); err != nil {
		return ReconciliationRun{}, err
	}
	for _, difference := range differences {
		providerJSON, _ := json.Marshal(difference.Provider)
		internalJSON, _ := json.Marshal(difference.Internal)
		_, err = tx.Exec(ctx, `INSERT INTO reconciliation_cases(id,reconciliation_run_id,payment_order_id,merchant_order_no,case_type,status,provider_snapshot,internal_snapshot,created_at) VALUES($1,$2,$3,$4,$5,'OPEN',$6,$7,$8)`, uuid.Must(uuid.NewV7()), run.ID, difference.PaymentOrderID, difference.MerchantOrderNo, difference.CaseType, providerJSON, internalJSON, now)
		if err != nil {
			return ReconciliationRun{}, err
		}
	}
	return run, tx.Commit(ctx)
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
