package billing

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const paymentNotificationRetryDelay = `make_interval(secs=>LEAST(300,power(2,LEAST(processing_attempts,8))::integer))`
const unmatchedPaymentNotificationAttemptLimit = 12

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
		if err := w.processBatch(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "payment notification inbox batch failed", "task", "payment-notification-inbox", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *PaymentNotificationWorker) processBatch(ctx context.Context) error {
	rows, err := w.payments.repository.pool.Query(ctx, `SELECT notification_id FROM payment_notifications WHERE result='RECEIVED' AND (next_attempt_at IS NULL OR next_attempt_at<=now()) ORDER BY received_at,notification_id LIMIT 200`)
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
		if err := w.payments.processPersistedNotification(ctx, id, true); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			errorCode := paymentNotificationErrorCode(err)
			attempts, retryAt, recordErr := w.recordFailure(ctx, id, errorCode)
			if errors.Is(recordErr, pgx.ErrNoRows) {
				continue // The notification was processed or rescheduled after selection.
			}
			if recordErr != nil {
				return recordErr
			}
			slog.ErrorContext(ctx, "payment notification processing failed",
				"task", "payment-notification-inbox",
				"notification_id", id,
				"error_code", errorCode,
				"processing_attempts", attempts,
				"next_attempt_at", retryAt,
			)
		}
	}
	return nil
}

func (w *PaymentNotificationWorker) recordFailure(ctx context.Context, id uuid.UUID, errorCode string) (int, time.Time, error) {
	now := w.payments.now()
	var attempts int
	var retryAt time.Time
	err := w.payments.repository.pool.QueryRow(ctx, `UPDATE payment_notifications
SET processing_attempts=processing_attempts+1,
    last_attempt_at=$2::timestamptz,
    next_attempt_at=$2::timestamptz+`+paymentNotificationRetryDelay+`,
    last_error_code=$3
WHERE notification_id=$1 AND result='RECEIVED'
  AND (next_attempt_at IS NULL OR next_attempt_at<=now())
RETURNING processing_attempts,next_attempt_at`, id, now, errorCode).Scan(&attempts, &retryAt)
	return attempts, retryAt, err
}

// The inbox row is locked and still due. Update its retry state and create any
// manual case in the same transaction, so either both persist or neither does.
func (p *PaymentCoordinator) recordUnmatchedPaymentNotification(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	const reason = "PAYMENT_ORDER_NOT_FOUND"
	now := p.now()
	var attempts int
	var result string
	err := tx.QueryRow(ctx, `UPDATE payment_notifications
SET processing_attempts=processing_attempts+1,
    last_attempt_at=$2::timestamptz,
    last_error_code=$3,
    result=CASE WHEN processing_attempts+1 >= $4 THEN 'MANUAL_REVIEW' ELSE result END,
    processed_at=CASE WHEN processing_attempts+1 >= $4 THEN $2::timestamptz ELSE NULL END,
    next_attempt_at=CASE WHEN processing_attempts+1 >= $4 THEN NULL ELSE $2::timestamptz+`+paymentNotificationRetryDelay+` END
WHERE notification_id=$1
RETURNING processing_attempts,result`, id, now, reason, unmatchedPaymentNotificationAttemptLimit).Scan(&attempts, &result)
	if err != nil {
		return err
	}
	if result == "MANUAL_REVIEW" {
		if _, err := createPaymentAnomalyWithinTransaction(ctx, tx, "PAYMENT_FACT_CONFLICT", "P1",
			"payment-notification:"+id.String()+":missing-order", paymentAnomalyReference{},
			map[string]any{"notification_id": id, "reason": reason}, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if result == "MANUAL_REVIEW" {
		slog.WarnContext(ctx, "unmatched payment notification requires manual review", "task", "payment-notification-inbox",
			"notification_id", id, "error_code", reason, "processing_attempts", attempts)
	}
	return nil
}

func paymentNotificationErrorCode(err error) string {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		code := "POSTGRES_" + postgresError.Code
		if constraint := strings.TrimSpace(postgresError.ConstraintName); constraint != "" {
			code += "_" + constraint
		}
		if len(code) > 160 {
			return code[:160]
		}
		return code
	}
	switch {
	case errors.Is(err, ErrConflict):
		return "BILLING_CONFLICT"
	case errors.Is(err, ErrForbiddenPayment):
		return "PAYMENT_CALLBACK_FORBIDDEN"
	default:
		return "PROCESSING_ERROR"
	}
}
