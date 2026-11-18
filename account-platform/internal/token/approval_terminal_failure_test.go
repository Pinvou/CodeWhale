package token

import (
	"context"
	"testing"
)

// Broker ACK and external HTTP authorities are controlled here. This exercises
// the production consumer, including the one-way ACK boundary; it does not
// simulate redelivery after a successful ACK to conceal missing metadata.
func TestTokenAuthorizationACKSuccessTerminalWriteFailure(t *testing.T) {
	for _, wire := range []string{"legacy", "approval"} {
		t.Run(wire, func(t *testing.T) {
			fixture, server := newTokenAuthorizationHTTPFixture(t, wire)
			defer server.Close()
			fixture.failStage = "terminal-write-unavailable"
			consumer, err := NewTokenAuthorizationCommandConsumer(fixture.event.CommandType, fixture.binding.Durable,
				server.URL, server.URL, "token-workload", &approvalActivationStub{state: "ACTIVE", epoch: 1})
			if err != nil {
				t.Fatal(err)
			}
			delivery := fixture.delivery(1)
			consumer.Handle(context.Background(), delivery)
			if delivery.ack != 1 || delivery.nak != 0 || delivery.publish != 0 ||
				fixture.state.State != "RESULT_CONFIRMED" || fixture.domainCalls != 1 || fixture.begins != 1 ||
				!fixture.completePersisted || fixture.resultWrites != 1 || fixture.terminals != 1 {
				t.Fatalf("ACK failure window concealed: ack=%d nak=%d publish=%d state=%s domain=%d begin=%d complete=%t result=%d terminal=%d",
					delivery.ack, delivery.nak, delivery.publish, fixture.state.State, fixture.domainCalls, fixture.begins,
					fixture.completePersisted, fixture.resultWrites, fixture.terminals)
			}
		})
	}
}
