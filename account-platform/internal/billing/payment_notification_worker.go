package billing

import (
	"context"
	"time"

	"github.com/google/uuid"
)

type PaymentNotificationWorker struct {
	payments *PaymentCoordinator
	interval time.Duration
}

func NewPaymentNotificationWorker(payments *PaymentCoordinator, interval time.Duration) *PaymentNotificationWorker {
	if interval < 25*time.Millisecond {
		interval = 25 * time.Millisecond
	}
	return &PaymentNotificationWorker{payments: payments, interval: interval}
}

func (w *PaymentNotificationWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		_ = w.processBatch(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *PaymentNotificationWorker) processBatch(ctx context.Context) error {
	rows, err := w.payments.repository.pool.Query(ctx, `SELECT notification_id FROM payment_notifications WHERE result='RECEIVED' ORDER BY received_at,notification_id LIMIT 200`)
	if err != nil {
		return err
	}
	ids := make([]uuid.UUID, 0, 200)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := w.payments.ProcessPersistedNotification(ctx, id); err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}
