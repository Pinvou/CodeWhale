package token

import (
	"context"
	"time"
)

type ReplayRetentionWorker struct {
	repository interface {
		CleanupReplayRetention(context.Context) error
		RecoverUncertainDispatches(context.Context) error
	}
	interval time.Duration
}

func NewReplayRetentionWorker(repository interface {
	CleanupReplayRetention(context.Context) error
	RecoverUncertainDispatches(context.Context) error
}, interval time.Duration) *ReplayRetentionWorker {
	if interval < 10*time.Second {
		interval = time.Hour
	}
	return &ReplayRetentionWorker{repository: repository, interval: interval}
}

func (w *ReplayRetentionWorker) Run(ctx context.Context) {
	_ = w.repository.RecoverUncertainDispatches(ctx)
	_ = w.repository.CleanupReplayRetention(ctx)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = w.repository.RecoverUncertainDispatches(ctx)
			_ = w.repository.CleanupReplayRetention(ctx)
		}
	}
}
