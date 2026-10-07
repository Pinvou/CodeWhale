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
}

// Breaker isolates state by provider + capability/action + route. Business
// rejections remain observable samples but never contribute to failures.
type Breaker struct {
	mu    sync.Mutex
	now   func() time.Time
	items map[string]*circuit
}

func NewBreaker() *Breaker {
	return &Breaker{now: func() time.Time { return time.Now().UTC() }, items: map[string]*circuit{}}
}

func Key(provider, capability, route string) string {
	return fmt.Sprintf("%s\x00%s\x00%s", provider, capability, route)
}

func (b *Breaker) Before(key string) error {
	return b.before(key, true)
}

// BeforeHighRisk never turns an original payment, refund, or model dispatch
// into a half-open probe after an uncertain result. Recovery must use a
// separate read-only capability probe or explicit review workflow.
func (b *Breaker) BeforeHighRisk(key string) error {
	return b.before(key, false)
}

func (b *Breaker) before(key string, allowHalfOpen bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	item := b.item(key)
	if item.openUntil.IsZero() {
		return nil
	}
	if now.Before(item.openUntil) {
		return &CircuitOpenError{RetryAfter: item.openUntil.Sub(now)}
	}
	if !allowHalfOpen {
		return &CircuitOpenError{RetryAfter: time.Second}
	}
	if item.halfOpenProbes >= 3 {
		return &CircuitOpenError{RetryAfter: time.Second}
	}
	item.halfOpenProbes++
	return nil
}

func (b *Breaker) Record(key string, outcome Outcome) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	item := b.item(key)
	if !item.openUntil.IsZero() && !now.Before(item.openUntil) {
		if outcome == Success {
			item.halfOpenSuccesses++
			if item.halfOpenSuccesses >= 3 {
				*item = circuit{}
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
	item.halfOpenProbes = 0
	item.halfOpenSuccesses = 0
}
