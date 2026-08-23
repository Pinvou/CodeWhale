package resilience

import (
	"errors"
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
