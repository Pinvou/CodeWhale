package resilience

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

var ErrCircuitOpen = errors.New("provider circuit is open")

type CircuitOpenError struct{ RetryAfter time.Duration }

func (e *CircuitOpenError) Error() string { return ErrCircuitOpen.Error() }
func (e *CircuitOpenError) Unwrap() error { return ErrCircuitOpen }

type Outcome uint8

const (
	Success Outcome = iota
	InfrastructureFailure
	BusinessRejection
	UncertainResult
)

type sample struct {
	at      time.Time
	outcome Outcome
}

type circuit struct {
	samples             []sample
	consecutiveFailures int
	openUntil           time.Time
	reopenCount         int
	halfOpenProbes      int
	halfOpenSuccesses   int
	generation          uint64
	pendingProbes       map[uint64]struct{}
}

// Breaker isolates state by provider + capability/action + route. Business
// rejections remain observable samples but never contribute to failures.
type Breaker struct {
	mu         sync.Mutex
	now        func() time.Time
	items      map[string]*circuit
	nextPermit uint64
}

func NewBreaker() *Breaker {
	return &Breaker{now: func() time.Time { return time.Now().UTC() }, items: map[string]*circuit{}}
}

func Key(provider, capability, route string) string {
	return fmt.Sprintf("%s\x00%s\x00%s", provider, capability, route)
}

func (b *Breaker) Before(key string) error {
	_, err := b.admit(key, true, false)
	return err
}

// BeforeHighRisk never turns an original payment, refund, or model dispatch
// into a half-open probe after an uncertain result. Recovery must use a
// separate read-only capability probe or explicit review workflow.
func (b *Breaker) BeforeHighRisk(key string) error {
	_, err := b.admit(key, false, false)
	return err
}

// BeforeReadOnly reserves a query probe. Cancel is only for an error before
// provider dispatch; it is idempotent and cannot release a newer generation.
// A dispatched call must use Record for its actual provider outcome instead.
func (b *Breaker) BeforeReadOnly(key string) (func(), error) {
	return b.admit(key, true, true)
}

func (b *Breaker) admit(key string, allowHalfOpen, tracked bool) (func(), error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	item := b.item(key)
	if item.openUntil.IsZero() {
		return func() {}, nil
	}
	if now.Before(item.openUntil) {
		return nil, &CircuitOpenError{RetryAfter: item.openUntil.Sub(now)}
	}
	if !allowHalfOpen {
		return nil, &CircuitOpenError{RetryAfter: time.Second}
	}
	if item.halfOpenProbes >= 3 {
		return nil, &CircuitOpenError{RetryAfter: time.Second}
	}
	item.halfOpenProbes++
	if !tracked {
		return func() {}, nil
	}
	b.nextPermit++
	permit, generation := b.nextPermit, item.generation
	if item.pendingProbes == nil {
		item.pendingProbes = map[uint64]struct{}{}
	}
	item.pendingProbes[permit] = struct{}{}
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if item.generation != generation {
			return
		}
		if _, exists := item.pendingProbes[permit]; !exists {
			return
		}
		delete(item.pendingProbes, permit)
		item.halfOpenProbes--
	}, nil
}

func (b *Breaker) Record(key string, outcome Outcome) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	item := b.item(key)
	if !item.openUntil.IsZero() && !now.Before(item.openUntil) {
		// A definite business rejection also proves the provider responded.
		// It must complete the probe rather than exhaust its admission budget.
		if outcome == Success || outcome == BusinessRejection {
			item.halfOpenSuccesses++
			if item.halfOpenSuccesses >= 3 {
				*item = circuit{generation: item.generation + 1}
			}
			return
		}
		if outcome == InfrastructureFailure || outcome == UncertainResult {
			b.open(item, now)
		}
		return
	}
	if outcome == InfrastructureFailure || outcome == UncertainResult {
		item.consecutiveFailures++
	} else if outcome == Success {
		item.consecutiveFailures = 0
	}
	item.samples = append(item.samples, sample{at: now, outcome: outcome})
	cutoff := now.Add(-time.Minute)
	first := 0
	for first < len(item.samples) && item.samples[first].at.Before(cutoff) {
		first++
	}
	item.samples = item.samples[first:]
	eligible, failures := 0, 0
	for _, value := range item.samples {
		if value.outcome == BusinessRejection {
			continue
		}
		eligible++
		if value.outcome == InfrastructureFailure || value.outcome == UncertainResult {
			failures++
		}
	}
	if item.consecutiveFailures >= 5 || eligible >= 20 && failures*2 >= eligible {
		b.open(item, now)
	}
}

func (b *Breaker) item(key string) *circuit {
	item := b.items[key]
	if item == nil {
		item = &circuit{}
		b.items[key] = item
	}
	return item
}

func (b *Breaker) open(item *circuit, now time.Time) {
	backoff := 30 * time.Second * time.Duration(1<<min(item.reopenCount, 5))
	item.reopenCount++
	item.openUntil = now.Add(min(backoff, 15*time.Minute))
	item.generation++
	item.pendingProbes = nil
	item.halfOpenProbes = 0
	item.halfOpenSuccesses = 0
}
