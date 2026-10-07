package token

import (
	"context"
	"errors"
	"testing"
	"time"
)

type rotationRepositoryFake struct {
	results []SecretRotationResult
	calls   int
	cancel  context.CancelFunc
}

func (f *rotationRepositoryFake) RotateSecretKey(context.Context, int) (SecretRotationResult, error) {
	if f.calls >= len(f.results) {
		return SecretRotationResult{}, errors.New("unexpected rotation call")
	}
	result := f.results[f.calls]
	f.calls++
	if result.Remaining == 0 && f.cancel != nil {
		f.cancel()
	}
	return result, nil
}

func TestSecretRotationWorkerDrainsAllBatchesBeforeOldKeyRetirement(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	repository := &rotationRepositoryFake{
		results: []SecretRotationResult{
			{ActiveKeyID: "kms-key-v2", Rotated: 100, Remaining: 3},
			{ActiveKeyID: "kms-key-v2", Rotated: 3, Remaining: 0},
		},
		cancel: cancel,
	}
	worker, err := NewSecretRotationWorker(repository, time.Minute, 100)
	if err != nil {
		t.Fatal(err)
	}
	worker.Run(ctx)
	if repository.calls != 2 {
		t.Fatalf("expected two rotation batches, got %d", repository.calls)
	}
}

func TestSecretRotationWorkerRejectsUnsafeConfiguration(t *testing.T) {
	repository := &rotationRepositoryFake{}
	for _, input := range []struct {
		interval time.Duration
		batch    int
	}{{time.Second, 100}, {time.Minute, 0}, {25 * time.Hour, 100}, {time.Minute, 1001}} {
		if _, err := NewSecretRotationWorker(repository, input.interval, input.batch); err == nil {
			t.Fatalf("accepted unsafe rotation configuration: %#v", input)
		}
	}
}
