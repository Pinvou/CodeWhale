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

func (w *ReplayRetentionWorker) runCycle(ctx context.Context) bool {
	if err := w.repository.RecoverUncertainDispatches(ctx); err != nil && ctx.Err() == nil {
		// Database errors can contain private values. Report a stable operational
		// code rather than the raw error, request IDs or encrypted payload data.
		w.logger.ErrorContext(ctx, "uncertain dispatch recovery failed", "error_code", "UNCERTAIN_DISPATCH_RECOVERY_RETRY_PENDING")
	}
	return w.runCleanup(ctx)
}

func (w *ReplayRetentionWorker) runCleanup(ctx context.Context) bool {
	var more bool
	var err error
	if bounded, ok := w.repository.(interface {
		cleanupReplayRetentionBatch(context.Context) (bool, error)
	}); ok {
		more, err = bounded.cleanupReplayRetentionBatch(ctx)
	} else {
		err = w.repository.CleanupReplayRetention(ctx)
	}
	if err != nil && ctx.Err() == nil {
		w.logger.ErrorContext(ctx, "replay retention cleanup failed", "error_code", "REPLAY_RETENTION_CLEANUP_RETRY_PENDING")
	}
	return err == nil && more
}

func (w *ReplayRetentionWorker) Run(ctx context.Context) {
	more := w.runCycle(ctx)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		var catchup <-chan time.Time
		if more {
			catchup = time.After(time.Second)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			more = w.runCycle(ctx)
		case <-catchup:
			// Only retention catches up; uncertain dispatch recovery keeps its
			// configured periodic cadence and financial recovery contract.
			more = w.runCleanup(ctx)
		}
	}
}
