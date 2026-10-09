package resilience

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCircuitOpensForThresholdsAndExcludesBusinessRejections(t *testing.T) {
	now := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	b := NewBreaker()
	b.now = func() time.Time { return now }
	key := Key("provider", "MODEL_DISPATCH", "route-a")
	for range 20 {
		b.Record(key, BusinessRejection)
	}
	if err := b.Before(key); err != nil {
		t.Fatal("business rejections opened the circuit")
	}
	for range 4 {
		b.Record(key, InfrastructureFailure)
	}
	if err := b.Before(key); err != nil {
		t.Fatal("circuit opened before five consecutive failures")
	}
	b.Record(key, UncertainResult)
	if err := b.Before(key); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected open circuit, got %v", err)
	}
}

func TestConcurrentHalfOpenAdmissionAndOutcomes(t *testing.T) {
	for _, failure := range []bool{false, true} {
		now := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
		b := NewBreaker()
		b.now = func() time.Time { return now }
		key := Key("provider", "READ_STATUS", "concurrent")
		for range 5 {
			b.Record(key, InfrastructureFailure)
		}
		now = now.Add(30 * time.Second)
		var admitted atomic.Int32
		var group sync.WaitGroup
		start := make(chan struct{})
		for range 32 {
			group.Go(func() {
				<-start
				if err := b.Before(key); err == nil {
					admitted.Add(1)
				} else if !errors.Is(err, ErrCircuitOpen) {
					t.Errorf("admission error: %v", err)
				}
			})
		}
		close(start)
		group.Wait()
		if admitted.Load() != 3 {
			t.Fatalf("admitted=%d, want 3", admitted.Load())
		}
		for index := range 3 {
			group.Go(func() {
				outcome := BusinessRejection
				if failure && index == 0 {
					outcome = InfrastructureFailure
				}
				b.Record(key, outcome)
			})
		}
		group.Wait()
		err := b.BeforeHighRisk(key)
		if failure && !errors.Is(err, ErrCircuitOpen) {
			t.Fatalf("failed probe reopened circuit incorrectly: %v", err)
		}
		if !failure && err != nil {
			t.Fatalf("healthy concurrent probes did not close: %v", err)
		}
	}
}

func TestCircuitUsesThreeHalfOpenProbesAndExponentialReopen(t *testing.T) {
	now := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	b := NewBreaker()
	b.now = func() time.Time { return now }
	key := Key("provider", "REFUND", "route")
	for range 5 {
		b.Record(key, InfrastructureFailure)
	}
	now = now.Add(30 * time.Second)
	for range 3 {
		if err := b.Before(key); err != nil {
			t.Fatal(err)
		}
		b.Record(key, Success)
	}
	if err := b.Before(key); err != nil {
		t.Fatal("three successful probes must close the circuit")
	}
	for range 5 {
		b.Record(key, InfrastructureFailure)
	}
	now = now.Add(30 * time.Second)
	if err := b.Before(key); err != nil {
		t.Fatal(err)
	}
	b.Record(key, InfrastructureFailure)
	var open *CircuitOpenError
	if err := b.Before(key); !errors.As(err, &open) || open.RetryAfter < time.Minute-time.Second {
		t.Fatalf("expected exponential reopen, got %v", err)
	}
}

func TestHighRiskOperationNeverBecomesAutomaticHalfOpenProbe(t *testing.T) {
	now := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	b := NewBreaker()
	b.now = func() time.Time { return now }
	key := Key("provider", "REFUND", "route")
	for range 5 {
		b.Record(key, UncertainResult)
	}
	now = now.Add(30 * time.Second)
	if err := b.BeforeHighRisk(key); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("high-risk request became a half-open probe: %v", err)
	}
}

func TestHalfOpenBusinessRejectionsCompleteHealthyProbes(t *testing.T) {
	now := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	b := NewBreaker()
	b.now = func() time.Time { return now }
	key := Key("provider", "READ_STATUS", "route")
	for range 5 {
		b.Record(key, InfrastructureFailure)
	}
	now = now.Add(30 * time.Second)
	for range 3 {
		if err := b.Before(key); err != nil {
			t.Fatal(err)
		}
		b.Record(key, BusinessRejection)
	}
	if err := b.BeforeHighRisk(key); err != nil {
		t.Fatalf("healthy business responses left circuit stuck: %v", err)
	}
}
