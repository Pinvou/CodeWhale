package billing

import (
	"context"
	"time"
)

type PersonalHoldExpiryWorker struct {
	repository interface {
		ExpireHolds(context.Context, int) (int, error)
	}
	interval time.Duration
}

func NewPersonalHoldExpiryWorker(repository interface {
	ExpireHolds(context.Context, int) (int, error)
}, interval time.Duration) *PersonalHoldExpiryWorker {
	if interval <= 0 {
		interval = time.Minute
	}
	return &PersonalHoldExpiryWorker{repository, interval}
}
func (w *PersonalHoldExpiryWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		_, _ = w.repository.ExpireHolds(ctx, 100)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
