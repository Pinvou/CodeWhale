package billing

import (
	"context"
	"testing"
)

func TestExpiredHoldWorkerRejectsInvalidLimitBeforeRepository(t *testing.T) {
	w := NewExpiredHoldRecoveryWorker(nil, nil, 0)
	for _, limit := range []int{-1, 0, 1001} {
		if w.RunOnceLimit(context.Background(), limit) == nil {
			t.Fatal("invalid limit accepted")
		}
	}
}
