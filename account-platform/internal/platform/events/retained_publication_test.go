package events

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

type publicationBroker struct{ ids []string }

func (broker *publicationBroker) Publish(_ context.Context, _ string, _ []byte, id string) error {
	broker.ids = append(broker.ids, id)
	return nil
}

func TestRelayReplayUsesNewPublicationGenerationWithoutChangingEnvelope(t *testing.T) {
	event := testEvent(0)
	event.PublicationGeneration = 1
	first, err := event.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	store := &memoryStore{events: []Event{event}}
	broker := &publicationBroker{}
	relay := Relay{Store: store, Broker: broker, WorkerID: "fixture"}
	if _, err := relay.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.events[0].PublicationGeneration++
	if _, err := relay.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	second, err := store.events[0].Marshal()
	if err != nil || !bytes.Equal(first, second) || strings.Contains(string(second), "publication") {
		t.Fatal("local replay lease metadata changed the immutable business envelope")
	}
	if len(broker.ids) != 2 || broker.ids[0] == broker.ids[1] || broker.ids[0] != event.ID.String()+":1" || broker.ids[1] != event.ID.String()+":2" {
		t.Fatalf("replay retained a stale broker dedup ID: %v", broker.ids)
	}
}
