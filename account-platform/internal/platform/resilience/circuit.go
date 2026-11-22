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
const probeLease = 30 * time.Second
const windowBuckets = 61

type outcomeBucket struct {
	second             int64
	eligible, failures uint64
}
type circuit struct {
	window              [windowBuckets]outcomeBucket
	consecutiveFailures int
	openUntil           time.Time
	reopenCount         int
	halfOpenSuccesses   int
	generation          uint64
	pendingProbes       map[uint64]time.Time
}

// Breaker isolates provider + capability/action + route. The minute window
// uses fixed one-second buckets, including its partially elapsed oldest second.
// Business rejections do not enter the failure ratio or reset consecutive errors.
type Breaker struct {
	mu         sync.Mutex
	now        func() time.Time
	items      map[string]*circuit
	nextPermit uint64
}

// Permit belongs to exactly one admission and circuit episode. Complete an
// actual provider outcome with Record. Cancel discards an unused admission or
// an observation withheld by local cancellation/trust checks; it asserts no
// provider outcome. It must not replace a confirmed current provider failure.
// Read-only half-open permits expire after 30s even if their caller disappears.
type Permit struct {
	breaker        *Breaker
	item           *circuit
	generation, id uint64
	probe, done    bool
}

func NewBreaker() *Breaker { return &Breaker{now: time.Now, items: map[string]*circuit{}} }
func Key(provider, capability, route string) string {
	return fmt.Sprintf("%s\x00%s\x00%s", provider, capability, route)
}
func (b *Breaker) AdmitHighRisk(key string) (*Permit, error) { return b.admit(key, false, false) }
func (b *Breaker) AdmitReadOnly(key string) (*Permit, error) { return b.admit(key, true, false) }

// AdmitReadOnlyRecovery attaches an already trusted read-only health check to
// recovery only. A closed/unknown circuit returns nil without adding samples.
func (b *Breaker) AdmitReadOnlyRecovery(key string) (*Permit, error) {
	return b.admit(key, true, true)
}

// TrackAccepted binds the result of already admitted, one-shot work. It grants
// no admission or half-open authority. In-flight work must not lose its consumed
// authorization merely because another call opened the circuit meanwhile.
func (b *Breaker) TrackAccepted(key string) *Permit {
	b.mu.Lock()
	defer b.mu.Unlock()
	item := b.item(key)
	return &Permit{breaker: b, item: item, generation: item.generation}
}

// Before and BeforeReadOnly retain local admission compatibility. New callers
// must retain an Admit* permit: an unbound Record cannot complete a probe.
func (b *Breaker) Before(key string) error         { _, err := b.AdmitReadOnly(key); return err }
func (b *Breaker) BeforeHighRisk(key string) error { _, err := b.AdmitHighRisk(key); return err }
func (b *Breaker) BeforeReadOnly(key string) (func(), error) {
	p, err := b.AdmitReadOnly(key)
	if err != nil {
		return nil, err
	}
	return p.Cancel, nil
}
func (b *Breaker) admit(key string, allowHalfOpen, recoveryOnly bool) (*Permit, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	item := b.items[key]
	if recoveryOnly && (item == nil || item.openUntil.IsZero()) {
		return nil, nil
	}
	if item == nil {
		item = b.item(key)
	}
	p := &Permit{breaker: b, item: item, generation: item.generation}
	if item.openUntil.IsZero() {
		return p, nil
	}
	if now.Before(item.openUntil) {
		return nil, &CircuitOpenError{RetryAfter: item.openUntil.Sub(now)}
	}
	if !allowHalfOpen {
		return nil, &CircuitOpenError{RetryAfter: time.Second}
	}
	for id, until := range item.pendingProbes {
		if !now.Before(until) {
			delete(item.pendingProbes, id)
		}
	}
	if len(item.pendingProbes) >= 3 {
		return nil, &CircuitOpenError{RetryAfter: time.Second}
	}
	b.nextPermit++
	p.id, p.probe = b.nextPermit, true
	if item.pendingProbes == nil {
		item.pendingProbes = map[uint64]time.Time{}
	}
	item.pendingProbes[p.id] = now.Add(probeLease)
	return p, nil
}
func (p *Permit) Cancel()                { p.finish(Success, true) }
func (p *Permit) Record(outcome Outcome) { p.finish(outcome, false) }
func (p *Permit) finish(outcome Outcome, cancel bool) {
	if p == nil || p.breaker == nil || p.item == nil {
		return
	}
	b := p.breaker
	b.mu.Lock()
	defer b.mu.Unlock()
	if p.done {
		return
	}
	p.done = true
	item := p.item
	if item.generation != p.generation {
		return
	}
	now := b.now()
	if p.probe {
		until, exists := item.pendingProbes[p.id]
		delete(item.pendingProbes, p.id)
		if !exists || !now.Before(until) || cancel {
			return
		}
		if outcome == Success || outcome == BusinessRejection {
			item.halfOpenSuccesses++
			if item.halfOpenSuccesses >= 3 {
				*item = circuit{generation: item.generation + 1}
			}
		} else if outcome == InfrastructureFailure || outcome == UncertainResult {
			b.open(item, now)
		}
		return
	}
	if !cancel && item.openUntil.IsZero() {
		b.observeClosed(item, now, outcome)
	}
}

// Record accepts legacy unattributed closed-state observations only. It never
// closes half-open state or escalates an open episode. Production calls retain
// Admit* permits so a late result cannot enter a later closed episode either.
func (b *Breaker) Record(key string, outcome Outcome) {
	b.mu.Lock()
	defer b.mu.Unlock()
	item := b.item(key)
	if item.openUntil.IsZero() {
		b.observeClosed(item, b.now(), outcome)
	}
}
func (b *Breaker) observeClosed(item *circuit, now time.Time, outcome Outcome) {
	if outcome == InfrastructureFailure || outcome == UncertainResult {
		item.consecutiveFailures++
	} else if outcome == Success {
		item.consecutiveFailures = 0
	}
	second := now.Unix()
	index := (second%windowBuckets + windowBuckets) % windowBuckets
	bucket := &item.window[index]
	if bucket.second != second {
		*bucket = outcomeBucket{second: second}
	}
	if outcome != BusinessRejection {
		bucket.eligible++
		if outcome == InfrastructureFailure || outcome == UncertainResult {
			bucket.failures++
		}
	}
	var eligible, failures uint64
	for _, v := range item.window {
		if v.second >= second-60 && v.second <= second {
			eligible += v.eligible
			failures += v.failures
		}
	}
	if item.consecutiveFailures >= 5 || eligible >= 20 && failures >= eligible-failures {
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
	item.halfOpenSuccesses = 0
}
