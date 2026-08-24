package events

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
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
	traceIDs    []string
}

func (broker *memoryBroker) Publish(ctx context.Context, subject string, _ []byte, _ string) error {
	broker.published = append(broker.published, subject)
	broker.traceIDs = append(broker.traceIDs, observability.TraceID(ctx))
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
	if broker.traceIDs[0] != store.events[0].TraceID {
		t.Fatalf("relay trace ID = %q, want %q", broker.traceIDs[0], store.events[0].TraceID)
	}
}

func TestDeliveryErrorsNeverPersistOriginalMessage(t *testing.T) {
	const marker = "secret-token-in-provider-error"
	if message := safeDeliveryError(errors.New(marker)); message != "EVENT_DELIVERY_FAILED" || strings.Contains(message, marker) {
		t.Fatalf("unsafe delivery error = %q", message)
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

func TestRelayRejectsSubjectOutsideSourcePermission(t *testing.T) {
	store := &memoryStore{events: []Event{{ID: uuid.New(), Type: "billing.refund.requested.v1", AggregateID: uuid.New(), AggregateVersion: 1, TraceID: "trace", Payload: []byte(`{}`)}}}
	broker := &memoryBroker{}
	result, err := (Relay{Store: store, Broker: broker, WorkerID: "account-relay", AllowedSubjects: []string{"account.>"}}).RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Published != 0 || result.Failed != 1 || len(broker.published) != 0 {
		t.Fatalf("cross-domain subject escaped relay: %#v %#v", result, broker.published)
	}
}

func TestAggregateVersionDecisionRejectsGapsAndIgnoresStaleEvents(t *testing.T) {
	if DecideVersion(false, 0, 1) != Apply || DecideVersion(true, 7, 6) != Stale || DecideVersion(true, 7, 7) != Stale || DecideVersion(true, 7, 9) != Gap || DecideVersion(true, 7, 8) != Apply {
		t.Fatal("aggregate version decisions are not deterministic")
	}
}

func TestDeadLetterDigestBindsTheImmutableEnvelope(t *testing.T) {
	event := testEvent(12)
	first, err := digestEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := digestEvent(event)
	if first != second || len(first) != 64 {
		t.Fatalf("event digest is not stable: %q %q", first, second)
	}
	event.Payload = json.RawMessage(`{"status":"DISABLED"}`)
	changed, _ := digestEvent(event)
	if changed == first {
		t.Fatal("event digest did not bind the payload")
	}
}

func TestReplayRequestHashSupportsRetryButRejectsSemanticChange(t *testing.T) {
	request := ReplayRequest{EventID: uuid.New(), ExpectedDigest: strings.Repeat("a", 64), IdempotencyKey: "stable-replay-key", Reason: "approved recovery after root cause fixed"}
	identity := serviceauth.Identity{ActorType: "SERVICE", Service: "event-replay", JobType: "EVENT_REPLAY", JobRunID: "first-run"}
	first := replayRequestHash(request, identity)
	identity.JobRunID = "retry-run"
	if replayRequestHash(request, identity) != first {
		t.Fatal("job retry changed the semantic command fingerprint")
	}
	request.Reason = "different audited recovery decision"
	if replayRequestHash(request, identity) == first {
		t.Fatal("semantic change reused a replay request fingerprint")
	}
}
