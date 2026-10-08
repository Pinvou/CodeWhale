package events

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"go.opentelemetry.io/otel/codes"
)

type Broker interface {
	Publish(context.Context, string, []byte, string) error
}

type OutboxStore interface {
	// Claim must filter allowed subjects before taking any delivery lease.
	Claim(context.Context, string, int, time.Duration, []string) ([]Event, error)
	MarkPublished(context.Context, string, Event) error
	MarkFailed(context.Context, string, Event, time.Time, string) error
	MarkDeadLettered(context.Context, string, Event, string) error
}

type Relay struct {
	Store           OutboxStore
	Broker          Broker
	WorkerID        string
	BatchSize       int
	Lease           time.Duration
	MaxAttempts     int
	AllowedSubjects []string
	Now             func() time.Time
}

type RelayResult struct {
	Claimed      int
	Published    int
	Failed       int
	DeadLettered int
}

func (relay Relay) RunOnce(ctx context.Context) (RelayResult, error) {
	if relay.Store == nil || relay.Broker == nil || relay.WorkerID == "" {
		return RelayResult{}, errors.New("event relay is not configured")
	}
	if !validSubjectScope(relay.AllowedSubjects) {
		return RelayResult{}, errors.New("event relay subject scope is invalid")
	}
	if relay.BatchSize < 1 || relay.BatchSize > 1000 {
		relay.BatchSize = 100
	}
	if relay.Lease <= 0 {
		relay.Lease = 30 * time.Second
	}
	if relay.MaxAttempts < 1 {
		relay.MaxAttempts = 12
	}
	if relay.Now == nil {
		relay.Now = func() time.Time { return time.Now().UTC() }
	}
	events, err := relay.Store.Claim(ctx, relay.WorkerID, relay.BatchSize, relay.Lease, append([]string(nil), relay.AllowedSubjects...))
	result := RelayResult{Claimed: len(events)}
	if err != nil {
		return result, err
	}
	// A store violating the claim contract is a configuration failure. Check
	// the whole batch before publishing or changing any event's retry state.
	for _, event := range events {
		if !subjectAllowed(relay.AllowedSubjects, event.Type) {
			return result, errors.New("event store claimed a subject outside the relay scope")
		}
	}
	for _, event := range events {
		eventContext := observability.ContextForTrace(ctx, event.ID.String(), event.TraceID)
		eventContext, span := observability.StartSpan(eventContext, "event.relay.publish")
		body, marshalErr := event.Marshal()
		if marshalErr == nil {
			marshalErr = relay.Broker.Publish(eventContext, event.Type, body, publicationID(event))
		}
		if marshalErr == nil {
			if err := relay.Store.MarkPublished(eventContext, relay.WorkerID, event); err != nil {
				span.SetStatus(codes.Error, "EVENT_ACKNOWLEDGEMENT_FAILED")
				span.End()
				return result, err
			}
			result.Published++
			span.End()
			continue
		}
		span.SetStatus(codes.Error, safeDeliveryError(marshalErr))
		nextAttempt := event.Attempts + 1
		message := safeDeliveryError(marshalErr)
		if nextAttempt >= relay.MaxAttempts {
			deadLetterSubject := "account-platform.dead-letter." + event.Type
			if err := relay.Broker.Publish(eventContext, deadLetterSubject, body, publicationID(event)+":dead-letter"); err == nil {
				if err := relay.Store.MarkDeadLettered(eventContext, relay.WorkerID, event, message); err != nil {
					span.End()
					return result, err
				}
				result.DeadLettered++
				span.End()
				continue
			}
		}
		retryAt := relay.Now().Add(retryDelay(nextAttempt))
		if err := relay.Store.MarkFailed(eventContext, relay.WorkerID, event, retryAt, message); err != nil {
			span.End()
			return result, err
		}
		result.Failed++
		span.End()
	}
	return result, nil
}

func publicationID(event Event) string {
	return event.ID.String() + ":" + strconv.FormatInt(event.PublicationGeneration, 10)
}

func subjectAllowed(patterns []string, subject string) bool {
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == subject || (strings.HasSuffix(pattern, ".>") && strings.HasPrefix(subject, strings.TrimSuffix(pattern, ">"))) {
			return true
		}
	}
	return false
}

// The store and relay share a narrow grammar: exact subjects or one terminal
// .> subtree. No implicit all-domain scope or single-token wildcards.
func validSubjectScope(patterns []string) bool {
	if len(patterns) == 0 || len(patterns) > 64 {
		return false
	}
	for _, pattern := range patterns {
		if pattern != strings.TrimSpace(pattern) || len(pattern) > 160 {
			return false
		}
		literal := strings.TrimSuffix(pattern, ".>")
		for _, part := range strings.Split(literal, ".") {
			if part == "" || strings.ContainsAny(part, "*>/ \t\r\n") {
				return false
			}
		}
	}
	return true
}

func retryDelay(attempt int) time.Duration {
	seconds := math.Pow(2, float64(min(attempt, 9)))
	return time.Duration(seconds) * time.Second
}

func safeDeliveryError(err error) string {
	if err == nil {
		return "EVENT_DELIVERY_FAILED"
	}
	message := strings.ToLower(fmt.Sprintf("%T", err))
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "EVENT_DELIVERY_TIMEOUT"
	case strings.Contains(message, "nats"):
		return "EVENT_BROKER_UNAVAILABLE"
	default:
		return "EVENT_DELIVERY_FAILED"
	}
}
