package token

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type rotationRepositoryFake struct {
	results []SecretRotationResult
	calls   int
	cancel  context.CancelFunc
}

type rotationOutcomeFake struct {
	result SecretRotationResult
	err    error
	calls  int
}

func (f *rotationOutcomeFake) RotateSecretKey(context.Context, int) (SecretRotationResult, error) {
	f.calls++
	return f.result, f.err
}

func TestSecretRotationWorkerStopsBoundedUnconfirmedSweep(t *testing.T) {
	for _, result := range []SecretRotationResult{
		{ActiveKeyID: "synthetic-key-must-not-log", Scanned: 1, Unconfirmed: 1, Remaining: 3},
		{ActiveKeyID: "synthetic-key-must-not-log", Scanned: 2, Rotated: 1, Unconfirmed: 1, Remaining: 3},
		{Scanned: 0, Remaining: 3, SweepComplete: true},
	} {
		fake := &rotationOutcomeFake{result: result}
		worker, err := NewSecretRotationWorker(fake, time.Minute, 1)
		if err != nil {
			t.Fatal(err)
		}
		worker.rotate(context.Background())
		if fake.calls != 1 {
			t.Fatalf("unconfirmed or completed sweep retried within one tick: %d", fake.calls)
		}
	}
}

func TestSecretRotationWorkerDiagnosticsNeverExposeProviderOrKey(t *testing.T) {
	var output bytes.Buffer
	original := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(original)
	for _, fake := range []*rotationOutcomeFake{
		{result: SecretRotationResult{ActiveKeyID: "synthetic-key-must-not-log", Scanned: 1, Unconfirmed: 1, Remaining: 1}},
		{err: errors.New("synthetic-provider-credential-must-not-log")},
	} {
		worker, err := NewSecretRotationWorker(fake, time.Minute, 1)
		if err != nil {
			t.Fatal(err)
		}
		worker.rotate(context.Background())
	}
	if !strings.Contains(output.String(), "SECRET_ROTATION_UNCONFIRMED") || !strings.Contains(output.String(), "SECRET_ROTATION_FAILED") || strings.Contains(output.String(), "must-not-log") {
		t.Fatal("rotation diagnostics missing or exposing private error/key", output.String())
	}
}

func TestSecretRotationGateCancellationDoesNotWaitForOtherCaller(t *testing.T) {
	r := &PostgresRepository{secretRotation: &secretRotationRuntime{gate: make(chan struct{}, 1)}}
	r.secretRotation.gate <- struct{}{}
	defer func() { <-r.secretRotation.gate }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.RotateSecretKey(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller reached storage or waited for gate: %v", err)
	}
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
