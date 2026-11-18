package account

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// RecoverApprovalDeliveryTerminals changes only already-confirmed delivery
// metadata. It never obtains a domain attempt or invokes an executor.
func (r *PostgresRepository) RecoverApprovalDeliveryTerminals(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 100 {
		return 0, ErrApprovalConsumerBindingInvalid
	}
	var recovered int
	err := r.pool.QueryRow(ctx, `SELECT public.recover_approval_authorization_terminals($1)`, limit).Scan(&recovered)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return 0, err
		}
		// Classification must not hide an independent storage fault merely
		// because the parent was canceled at the same time. No query is run
		// with this cancellation-free classification context.
		return 0, approvalConsumerError(context.WithoutCancel(ctx), err)
	}
	return recovered, nil
}

type approvalDeliveryTerminalRecoveryRepository interface {
	RecoverApprovalDeliveryTerminals(context.Context, int) (int, error)
}

type ApprovalDeliveryTerminalRecoveryWorker struct {
	repository approvalDeliveryTerminalRecoveryRepository
	interval   time.Duration
}

func NewApprovalDeliveryTerminalRecoveryWorker(repository approvalDeliveryTerminalRecoveryRepository, interval time.Duration) *ApprovalDeliveryTerminalRecoveryWorker {
	if interval <= 0 {
		interval = time.Minute
	}
	return &ApprovalDeliveryTerminalRecoveryWorker{repository: repository, interval: interval}
}

func (worker *ApprovalDeliveryTerminalRecoveryWorker) Run(ctx context.Context) {
	if worker == nil || worker.repository == nil {
		return
	}
	ticker := time.NewTicker(worker.interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		pass, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := worker.repository.RecoverApprovalDeliveryTerminals(pass, 100)
		cancel()
		if err != nil && !errors.Is(err, ctx.Err()) {
			slog.ErrorContext(ctx, "approval delivery terminal recovery failed", "error_code", "APPROVAL_DELIVERY_TERMINAL_RECOVERY_FAILED")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
