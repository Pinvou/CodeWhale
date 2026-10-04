package events

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInvalidEvent = errors.New("invalid event")
	ErrVersionGap   = errors.New("aggregate event version gap")
)

type Event struct {
	ID               uuid.UUID
	Type             string
	AggregateID      uuid.UUID
	AggregateVersion int64
	Payload          json.RawMessage
	TraceID          string
	OccurredAt       time.Time
	Attempts         int
	// Publication metadata is local to Outbox delivery, never the business envelope.
	PublicationGeneration int64
	LeaseUntil            time.Time
}

func (event Event) Validate() error {
	if event.ID == uuid.Nil || event.AggregateID == uuid.Nil || event.AggregateVersion < 1 || len(event.Payload) == 0 || strings.TrimSpace(event.TraceID) == "" {
		return ErrInvalidEvent
	}
	for _, part := range strings.Split(event.Type, ".") {
		if part == "" || part == "*" || part == ">" || strings.ContainsAny(part, " \t\r\n") {
			return ErrInvalidEvent
		}
	}
	return nil
}

func (event Event) Marshal() ([]byte, error) {
	if err := event.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		EventID          uuid.UUID       `json:"event_id"`
		EventType        string          `json:"event_type"`
		AggregateID      uuid.UUID       `json:"aggregate_id"`
		AggregateVersion int64           `json:"aggregate_version"`
		OccurredAt       time.Time       `json:"occurred_at"`
		TraceID          string          `json:"trace_id"`
		Payload          json.RawMessage `json:"payload"`
	}{event.ID, event.Type, event.AggregateID, event.AggregateVersion, event.OccurredAt.UTC(), event.TraceID, event.Payload})
}

func Unmarshal(body []byte) (Event, error) {
	var envelope struct {
		EventID          uuid.UUID       `json:"event_id"`
		EventType        string          `json:"event_type"`
		AggregateID      uuid.UUID       `json:"aggregate_id"`
		AggregateVersion int64           `json:"aggregate_version"`
		OccurredAt       time.Time       `json:"occurred_at"`
		TraceID          string          `json:"trace_id"`
		Payload          json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return Event{}, ErrInvalidEvent
	}
	event := Event{ID: envelope.EventID, Type: envelope.EventType, AggregateID: envelope.AggregateID, AggregateVersion: envelope.AggregateVersion, OccurredAt: envelope.OccurredAt, TraceID: envelope.TraceID, Payload: envelope.Payload}
	if err := event.Validate(); err != nil {
		return Event{}, err
	}
	return event, nil
}

type VersionDecision int

const (
	Apply VersionDecision = iota
	Stale
	Gap
)

func DecideVersion(hasCurrent bool, current, incoming int64) VersionDecision {
	if incoming < 1 {
		return Gap
	}
	if !hasCurrent {
		if incoming == 1 {
			return Apply
		}
		return Gap
	}
	if incoming <= current {
		return Stale
	}
	if incoming != current+1 {
		return Gap
	}
	return Apply
}
