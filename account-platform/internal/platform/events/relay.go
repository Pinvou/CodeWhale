package events

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"
)

type Broker interface {
	Publish(context.Context, string, []byte, string) error
}

type OutboxStore interface {
	Claim(context.Context, string, int, time.Duration) ([]Event, error)
	MarkPublished(context.Context, string, Event) error
	MarkFailed(context.Context, string, Event, time.Time, string) error
	MarkDeadLettered(context.Context, string, Event, string) error
}

type Relay struct {
	Store       OutboxStore
	Broker      Broker
	WorkerID    string
	BatchSize   int
	Lease       time.Duration
	MaxAttempts int
	Now         func() time.Time
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
	events, err := relay.Store.Claim(ctx, relay.WorkerID, relay.BatchSize, relay.Lease)
	result := RelayResult{Claimed: len(events)}
	if err != nil {
		return result, err
	}
	for _, event := range events {
		body, marshalErr := event.Marshal()
		if marshalErr == nil {
			marshalErr = relay.Broker.Publish(ctx, event.Type, body, publicationID(event))
		}
		if marshalErr == nil {
			if err := relay.Store.MarkPublished(ctx, relay.WorkerID, event); err != nil {
				return result, err
			}
			result.Published++
			continue
		}
		nextAttempt := event.Attempts + 1
		message := truncateError(marshalErr)
		if nextAttempt >= relay.MaxAttempts {
			deadLetterSubject := "account-platform.dead-letter." + event.Type
			if err := relay.Broker.Publish(ctx, deadLetterSubject, body, publicationID(event)+":dead-letter"); err == nil {
				if err := relay.Store.MarkDeadLettered(ctx, relay.WorkerID, event, message); err != nil {
					return result, err
				}
				result.DeadLettered++
				continue
			}
		}
		retryAt := relay.Now().Add(retryDelay(nextAttempt))
		if err := relay.Store.MarkFailed(ctx, relay.WorkerID, event, retryAt, message); err != nil {
			return result, err
		}
		result.Failed++
	}
	return result, nil
}

func publicationID(event Event) string {
	return event.ID.String() + ":" + strconv.FormatInt(event.PublicationGeneration, 10)
}

func retryDelay(attempt int) time.Duration {
	seconds := math.Pow(2, float64(min(attempt, 9)))
	return time.Duration(seconds) * time.Second
}

func truncateError(err error) string {
	if err == nil {
		return "unknown event delivery error"
	}
	message := fmt.Sprintf("%v", err)
	if len(message) > 512 {
		message = message[:512]
	}
	return message
}
