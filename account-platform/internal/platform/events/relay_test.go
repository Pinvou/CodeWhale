package events

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

type memoryStore struct {
	events       []Event
	published    []uuid.UUID
	failed       []uuid.UUID
	deadLettered []uuid.UUID
}

func (store *memoryStore) Claim(context.Context, string, int, time.Duration) ([]Event, error) {
	return store.events, nil
}
func (store *memoryStore) MarkPublished(_ context.Context, _ string, event Event) error {
	store.published = append(store.published, event.ID)
	return nil
}
func (store *memoryStore) MarkFailed(_ context.Context, _ string, event Event, _ time.Time, _ string) error {
	store.failed = append(store.failed, event.ID)
	return nil
}
func (store *memoryStore) MarkDeadLettered(_ context.Context, _ string, event Event, _ string) error {
	store.deadLettered = append(store.deadLettered, event.ID)
	return nil
}

type memoryBroker struct {
	failPrimary bool
	published   []string
}

func (broker *memoryBroker) Publish(_ context.Context, subject string, _ []byte, _ string) error {
	broker.published = append(broker.published, subject)
	if broker.failPrimary && subject == "account.membership.changed.v1" {
		return errors.New("NATS unavailable")
	}
	return nil
}

func testEvent(attempts int) Event {
	return Event{ID: uuid.Must(uuid.NewV7()), Type: "account.membership.changed.v1", AggregateID: uuid.Must(uuid.NewV7()), AggregateVersion: 1, Payload: json.RawMessage(`{"status":"ACTIVE"}`), TraceID: "trace-1", OccurredAt: time.Now().UTC(), Attempts: attempts}
}

func TestRelayPublishesEnvelopeAndAcknowledgesOutbox(t *testing.T) {
	store := &memoryStore{events: []Event{testEvent(0)}}
	broker := &memoryBroker{}
	result, err := (Relay{Store: store, Broker: broker, WorkerID: "worker-1"}).RunOnce(context.Background())
	if err != nil || result.Published != 1 || len(store.published) != 1 || broker.published[0] != store.events[0].Type {
		t.Fatalf("unexpected relay result: %#v, %v", result, err)
	}
}

func TestRelayRetriesWithoutDroppingOutboxEvent(t *testing.T) {
	store := &memoryStore{events: []Event{testEvent(0)}}
	result, err := (Relay{Store: store, Broker: &memoryBroker{failPrimary: true}, WorkerID: "worker-1"}).RunOnce(context.Background())
	if err != nil || result.Failed != 1 || len(store.failed) != 1 || len(store.published) != 0 {
		t.Fatalf("unexpected retry result: %#v, %v", result, err)
	}
}

func TestRelayMovesExhaustedEventToDeadLetterStream(t *testing.T) {
	store := &memoryStore{events: []Event{testEvent(11)}}
	broker := &memoryBroker{failPrimary: true}
	result, err := (Relay{Store: store, Broker: broker, WorkerID: "worker-1", MaxAttempts: 12}).RunOnce(context.Background())
	if err != nil || result.DeadLettered != 1 || len(store.deadLettered) != 1 || broker.published[1] != "account-platform.dead-letter.account.membership.changed.v1" {
		t.Fatalf("unexpected dead-letter result: %#v, %v, %#v", result, err, broker.published)
	}
}

func TestAggregateVersionDecisionRejectsGapsAndIgnoresStaleEvents(t *testing.T) {
	if DecideVersion(false, 0, 1) != Apply || DecideVersion(true, 7, 6) != Stale || DecideVersion(true, 7, 7) != Stale || DecideVersion(true, 7, 9) != Gap || DecideVersion(true, 7, 8) != Apply {
		t.Fatal("aggregate version decisions are not deterministic")
	}
}
