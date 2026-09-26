package token

import (
	"context"
	"testing"
	"time"
)

type usageHistoryBackfillRecorder struct {
	called chan struct{}
}

func (recorder *usageHistoryBackfillRecorder) BackfillUsageHistoryProjection(context.Context, int) (int, bool, error) {
	select {
	case recorder.called <- struct{}{}:
	default:
	}
	return 0, true, nil
}

func TestUsageHistoryProjectionWorkerRunsInitialBoundedBatch(t *testing.T) {
	recorder := &usageHistoryBackfillRecorder{called: make(chan struct{}, 1)}
	worker := NewUsageHistoryProjectionWorker(recorder, 500)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		worker.Run(ctx)
		close(done)
	}()
	select {
	case <-recorder.called:
		cancel()
		<-done
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("usage history projection worker did not run its initial batch")
	}
}
