package token

import (
	"context"
	"log/slog"
	"time"
)

type ReplayRetentionWorker struct {
	repository interface {
		CleanupReplayRetention(context.Context) error
		RecoverUncertainDispatches(context.Context) error
	}
	interval time.Duration
	logger   *slog.Logger
}

func NewReplayRetentionWorker(repository interface {
	CleanupReplayRetention(context.Context) error
	RecoverUncertainDispatches(context.Context) error
}, interval time.Duration) *ReplayRetentionWorker {
	if interval < 10*time.Second {
		interval = time.Hour
	}
	return &ReplayRetentionWorker{repository: repository, interval: interval, logger: slog.Default().With("component", "usage-replay-recovery")}
}

func (w *ReplayRetentionWorker) runCycle(ctx context.Context) {
	if err := w.repository.RecoverUncertainDispatches(ctx); err != nil && ctx.Err() == nil {
		// Database errors can contain private values. Report a stable operational
		// code rather than the raw error, request IDs or encrypted payload data.
		w.logger.ErrorContext(ctx, "uncertain dispatch recovery failed", "error_code", "UNCERTAIN_DISPATCH_RECOVERY_RETRY_PENDING")
	}
	_ = w.repository.CleanupReplayRetention(ctx)
}

func (w *ReplayRetentionWorker) Run(ctx context.Context) {
	w.runCycle(ctx)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.runCycle(ctx)
		}
	}
}
