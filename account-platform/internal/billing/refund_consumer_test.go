package billing

import (
	"context"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
)

func TestRefundCommandConsumerRejectsInvalidEventsBeforeProviderExecution(t *testing.T) {
	consumer := NewRefundCommandConsumer(&PaymentCoordinator{})
	if err := consumer.Handle(context.Background(), events.Event{Type: "billing.refund.succeeded.v1"}); err == nil {
		t.Fatal("unsupported refund event was accepted")
	}
	refundID := uuid.Must(uuid.NewV7())
	if err := consumer.Handle(context.Background(), events.Event{Type: "billing.refund.requested.v1", AggregateID: refundID, Payload: []byte(`{"refund_id":"00000000-0000-0000-0000-000000000000"}`)}); err == nil {
		t.Fatal("mismatched refund event target was accepted")
	}
}
