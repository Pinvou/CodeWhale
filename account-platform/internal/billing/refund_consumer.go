package billing

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
)

type RefundCommandConsumer struct {
	coordinator *PaymentCoordinator
}

func NewRefundCommandConsumer(coordinator *PaymentCoordinator) *RefundCommandConsumer {
	return &RefundCommandConsumer{coordinator: coordinator}
}

func (consumer *RefundCommandConsumer) Handle(ctx context.Context, event events.Event) error {
	if consumer == nil || consumer.coordinator == nil || event.Type != "billing.refund.requested.v1" {
		return errors.New("unsupported refund command event")
	}
	var payload struct {
		RefundID uuid.UUID `json:"refund_id"`
	}
	if json.Unmarshal(event.Payload, &payload) != nil || payload.RefundID == uuid.Nil || payload.RefundID != event.AggregateID {
		return errors.New("invalid refund command event")
	}
	return consumer.coordinator.ProcessRefund(ctx, payload.RefundID)
}
