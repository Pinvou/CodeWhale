package billing

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
)

type ApprovalCommandConsumer struct {
	service *Service
}

func NewApprovalCommandConsumer(service *Service) *ApprovalCommandConsumer {
	return &ApprovalCommandConsumer{service: service}
}

func (consumer *ApprovalCommandConsumer) Handle(ctx context.Context, event events.Event) error {
	if consumer == nil || consumer.service == nil || event.Type != "account.approval-command.authorized.v1" {
		return errors.New("unsupported approval authorization event")
	}
	var payload struct {
		ApprovalRequestID        uuid.UUID       `json:"approval_request_id"`
		CommandType              string          `json:"command_type"`
		TargetResourceType       string          `json:"target_resource_type"`
		TargetResourceID         string          `json:"target_resource_id"`
		CriticalResourceVersions json.RawMessage `json:"critical_resource_versions"`
		ExecutorIdentity         string          `json:"executor_identity"`
	}
	if json.Unmarshal(event.Payload, &payload) != nil || payload.ApprovalRequestID == uuid.Nil || payload.CommandType != "WALLET_ADJUSTMENT" || payload.TargetResourceType != "MANUAL_ADJUSTMENT" || payload.ExecutorIdentity != billingApprovalExecutorIdentity {
		return errors.New("unsupported or invalid approved command")
	}
	adjustmentID, err := uuid.Parse(strings.TrimSpace(payload.TargetResourceID))
	if err != nil {
		return errors.New("invalid approved command target")
	}
	_, err = consumer.service.executeApprovedAdjustmentCommand(ctx, adjustmentID, payload.ApprovalRequestID, event.ID.String(), payload.CriticalResourceVersions)
	if errors.Is(err, ErrApprovalSnapshotMismatch) || errors.Is(err, ErrApprovalNotExecutable) {
		return nil
	}
	return err
}
