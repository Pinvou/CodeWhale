package billing

import (
	"context"
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
)

type ApprovalCommandConsumer struct {
	service *Service
}

func NewApprovalCommandConsumer(service *Service) *ApprovalCommandConsumer {
	return &ApprovalCommandConsumer{service: service}
}

func (consumer *ApprovalCommandConsumer) Handle(ctx context.Context, event events.Event) error {
	_ = ctx
	_ = event
	// Authorization messages must enter through ConsumeAuthorization before
	// any generic Event decoding or physical-delivery retry policy is applied.
	// Keep this legacy method as an explicit fail-closed compatibility symbol.
	return errors.New("generic approval authorization consumption is forbidden")
}
