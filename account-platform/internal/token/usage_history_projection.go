package token

import (
	"context"
	"time"
)

// UsageHistoryProjectionWorker converts legacy usage rows into the sidecar
// projection in bounded transactions. The insert trigger independently keeps
// writes from old and new Gateway pods current during the backfill.
type UsageHistoryProjectionWorker struct {
	repository interface {
		BackfillUsageHistoryProjection(context.Context, int) (int, bool, error)
	}
	batchSize int
}

func NewUsageHistoryProjectionWorker(repository interface {
	BackfillUsageHistoryProjection(context.Context, int) (int, bool, error)
}, batchSize int) *UsageHistoryProjectionWorker {
	if batchSize < 1 || batchSize > 5000 {
		batchSize = 500
	}
	return &UsageHistoryProjectionWorker{repository: repository, batchSize: batchSize}
}

func (worker *UsageHistoryProjectionWorker) Run(ctx context.Context) {
	delay := time.Duration(0)
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		processed, complete, err := worker.repository.BackfillUsageHistoryProjection(ctx, worker.batchSize)
		switch {
		case err != nil:
			delay = 5 * time.Second
		case complete:
			delay = time.Minute
		case processed == 0:
			delay = 250 * time.Millisecond
		default:
			delay = 25 * time.Millisecond
		}
	}
}
