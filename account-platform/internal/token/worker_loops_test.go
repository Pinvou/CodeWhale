package token

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

type loopReconciliationStore struct {
	reconciliationStoreFake
	steps  []string
	claims int
	cancel context.CancelFunc
}

func (s *loopReconciliationStore) RecoverLegacyResponseStateSplits(context.Context, int) (int64, error) {
	s.steps = append(s.steps, "recover")
	return 0, errors.New("controlled recovery failure")
}

func (s *loopReconciliationStore) ClaimReconciliation(context.Context, int, time.Duration) ([]ReconciliationItem, error) {
	s.steps = append(s.steps, "claim")
	s.claims++
	if s.claims == 2 {
		s.cancel()
	}
	return nil, errors.New("controlled claim failure")
}

func TestUsageReconcilerLoopRetriesAndStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &loopReconciliationStore{cancel: cancel}
	worker := NewUsageReconciler(store, nil, nil, 15*time.Second)
	// Accelerate only this test's ticker; production interval policy is unchanged.
	worker.interval = time.Millisecond
	done := make(chan struct{})
	go func() {
		worker.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reconciler did not retry and exit after cancellation")
	}
	if store.claims < 2 || len(store.steps)%2 != 0 {
		t.Fatal("recovery/claim failures stopped the worker", store.steps)
	}
	for n := 0; n < len(store.steps); n += 2 {
		if !reflect.DeepEqual(store.steps[n:n+2], []string{"recover", "claim"}) {
			t.Fatal("claim ran before legacy recovery", store.steps)
		}
	}
}

type loopRetentionStore struct {
	steps    []string
	cleanups int
	cancel   context.CancelFunc
}

func (s *loopRetentionStore) RecoverUncertainDispatches(context.Context) error {
	s.steps = append(s.steps, "recover")
	return errors.New("controlled recovery failure")
}

func (s *loopRetentionStore) CleanupReplayRetention(context.Context) error {
	s.steps = append(s.steps, "cleanup")
	s.cleanups++
	if s.cleanups == 2 {
		s.cancel()
	}
	return errors.New("controlled cleanup failure")
}

func TestReplayRetentionLoopRetriesAndStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &loopRetentionStore{cancel: cancel}
	worker := NewReplayRetentionWorker(store, 15*time.Second)
	// Accelerate only this test's ticker; production interval policy is unchanged.
	worker.interval = time.Millisecond
	done := make(chan struct{})
	go func() {
		worker.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("retention worker did not retry and exit after cancellation")
	}
	if store.cleanups < 2 || len(store.steps)%2 != 0 {
		t.Fatal("repository failures stopped the worker", store.steps)
	}
	for n := 0; n < len(store.steps); n += 2 {
		if !reflect.DeepEqual(store.steps[n:n+2], []string{"recover", "cleanup"}) {
			t.Fatal("cleanup ran before dispatch recovery", store.steps)
		}
	}
}
