package resilience

import (
	"errors"
	"testing"
	"time"
)

func TestHalfOpenPermitsExpireWithoutLateOutcomeAuthority(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	b := NewBreaker()
	b.now = func() time.Time { return now }
	key := Key("provider", "MODEL_DISPATCH", "route")
	oldClosed, err := b.AdmitHighRisk(key)
	if err != nil {
		t.Fatal(err)
	}
	oldAfterRecovery, err := b.AdmitHighRisk(key)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		b.Record(key, InfrastructureFailure)
	}
	until := b.items[key].openUntil
	oldClosed.Record(InfrastructureFailure)
	if b.items[key].openUntil != until || b.items[key].reopenCount != 1 {
		t.Fatal("late closed call escalated the open episode")
	}
	now = now.Add(30 * time.Second)
	var abandoned []*Permit
	for range 3 {
		p, err := b.AdmitReadOnly(key)
		if err != nil {
			t.Fatal(err)
		}
		abandoned = append(abandoned, p)
	}
	if _, err = b.AdmitReadOnly(key); !errors.Is(err, ErrCircuitOpen) {
		t.Fatal("more than three live probes admitted", err)
	}
	now = now.Add(probeLease)
	abandoned[0].Record(Success)
	if b.items[key].halfOpenSuccesses != 0 {
		t.Fatal("expired result retained recovery authority before replacement")
	}
	current, err := b.AdmitReadOnly(key)
	if err != nil {
		t.Fatal("lost callers permanently exhausted probe budget", err)
	}
	for _, p := range abandoned {
		p.Record(Success)
		p.Cancel()
		p.Record(InfrastructureFailure)
	}
	if err = b.BeforeHighRisk(key); !errors.Is(err, ErrCircuitOpen) {
		t.Fatal("expired probes closed the circuit", err)
	}
	current.Record(Success)
	current.Record(Success)
	for range 2 {
		p, err := b.AdmitReadOnly(key)
		if err != nil {
			t.Fatal(err)
		}
		p.Record(BusinessRejection)
	}
	if err = b.BeforeHighRisk(key); err != nil {
		t.Fatal("three current independent responses did not recover", err)
	}
	for _, p := range abandoned {
		p.Record(InfrastructureFailure)
	}
	oldAfterRecovery.Record(InfrastructureFailure)
	if b.items[key].consecutiveFailures != 0 {
		t.Fatal("pre-open result entered later closed episode")
	}
	if err = b.BeforeHighRisk(key); err != nil {
		t.Fatal("old callback reopened recovered episode", err)
	}
}

func TestHalfOpenFailureIsBoundToOneEpisode(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	b := NewBreaker()
	b.now = func() time.Time { return now }
	key := Key("provider", "ORDER_QUERY", "route")
	for range 5 {
		b.Record(key, InfrastructureFailure)
	}
	now = now.Add(30 * time.Second)
	first, _ := b.AdmitReadOnly(key)
	late, _ := b.AdmitReadOnly(key)
	first.Record(UncertainResult)
	until, generation := b.items[key].openUntil, b.items[key].generation
	late.Record(InfrastructureFailure)
	b.Record(key, InfrastructureFailure)
	if b.items[key].openUntil != until || b.items[key].generation != generation || b.items[key].reopenCount != 2 {
		t.Fatal("late result escalated backoff")
	}
	now = until
	for range 3 {
		p, err := b.AdmitReadOnly(key)
		if err != nil {
			t.Fatal(err)
		}
		p.Record(Success)
	}
	if err := b.BeforeHighRisk(key); err != nil {
		t.Fatal("current episode did not recover", err)
	}
}

func TestReadOnlyRecoveryDoesNotCreateOrDiluteDispatchSamples(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	b := NewBreaker()
	b.now = func() time.Time { return now }
	key := Key("provider", "MODEL_DISPATCH", "route")
	if p, err := b.AdmitReadOnlyRecovery(key); p != nil || err != nil || len(b.items) != 0 {
		t.Fatal("routine health created circuit state", p, err)
	}
	for range 4 {
		b.Record(key, InfrastructureFailure)
		if p, err := b.AdmitReadOnlyRecovery(key); p != nil || err != nil {
			t.Fatal("closed health entered dispatch observations", p, err)
		}
	}
	b.Record(key, InfrastructureFailure)
	if _, err := b.AdmitReadOnlyRecovery(key); !errors.Is(err, ErrCircuitOpen) {
		t.Fatal("cooldown did not constrain recovery", err)
	}
	now = now.Add(30 * time.Second)
	for range 3 {
		p, err := b.AdmitReadOnlyRecovery(key)
		if err != nil || p == nil {
			t.Fatal(err)
		}
		p.Record(Success)
	}
	if err := b.BeforeHighRisk(key); err != nil {
		t.Fatal(err)
	}
	other := Key("provider", "MODEL_DISPATCH", "other-route")
	for range 5 {
		b.Record(other, InfrastructureFailure)
	}
	if err := b.BeforeHighRisk(key); err != nil {
		t.Fatal("other route was not isolated", err)
	}
}

func TestSecondBucketsRetainThresholdsAndExpireOldMinute(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	b := NewBreaker()
	b.now = func() time.Time { return now }
	key := Key("provider", "READ_STATUS", "route")
	for range 10000 {
		b.Record(key, Success)
	}
	now = now.Add(61 * time.Second)
	for range 9 {
		b.Record(key, InfrastructureFailure)
		b.Record(key, Success)
	}
	b.Record(key, InfrastructureFailure)
	if err := b.BeforeHighRisk(key); err != nil {
		t.Fatal("ratio evaluated before20 observations", err)
	}
	b.Record(key, Success)
	if err := b.BeforeHighRisk(key); !errors.Is(err, ErrCircuitOpen) {
		t.Fatal("old successes diluted current50percent failure ratio", err)
	}
}

func BenchmarkCircuitBoundedWindow(b *testing.B) {
	breaker := NewBreaker()
	key := Key("provider", "READ_STATUS", "route")
	breaker.Record(key, Success)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		breaker.Record(key, Success)
	}
}
