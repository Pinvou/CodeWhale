package events

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/nats-io/nats.go/jetstream"
)

// UpgradeLegacyTokenConsumers is opt-in and never creates/deletes a durable.
// All five original resources and every Token sibling are checked before the
// first update. The independently reviewed plan fixes creation identities and
// exact delivery/ACK positions; resume never captures replacement positions.
func UpgradeLegacyTokenConsumers(ctx context.Context, rawURL string, cfg NATSConfig, files ExistingBrokerResourceFiles, facts FreshApprovalControl, apply, resume bool) ([]byte, error) {
	return runLegacyTokenUpgrade(ctx, rawURL, cfg, files, facts, apply, resume, false)
}

// InspectLegacyTokenConsumers classifies a known original plan without UPDATE
// or an audit write. It remains usable when only a subset received an update.
func InspectLegacyTokenConsumers(ctx context.Context, rawURL string, cfg NATSConfig, files ExistingBrokerResourceFiles, facts FreshApprovalControl) ([]byte, error) {
	return runLegacyTokenUpgrade(ctx, rawURL, cfg, files, facts, false, false, true)
}

func runLegacyTokenUpgrade(ctx context.Context, rawURL string, cfg NATSConfig, files ExistingBrokerResourceFiles, facts FreshApprovalControl, apply, resume, inspect bool) ([]byte, error) {
	files = files.withAuthentication(cfg)
	plan, err := readBrokerPlan(files, facts, true)
	if err != nil {
		return nil, err
	}
	c, stream, err := openExistingBroker(ctx, rawURL, cfg, files.ApprovalConsumerResourceFiles)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if exactConsumerNames(ctx, stream, existingNames(), existingNames()) != nil || !planStreamMatches(ctx, stream, plan) {
		return nil, ErrNATSResources
	}
	legacy, current := tokenUpgradeConfigurations(), existingBrokerConfigurations()
	type update struct {
		configuration, expected jetstream.ConsumerConfig
		original                existingConsumerCapture
	}
	updates := []update{}
	currentCount := 0
	observed := []existingConsumerCapture{}
	for i, expected := range legacy {
		consumer, e := stream.Consumer(ctx, expected.Name)
		if e != nil {
			return nil, ErrNATSResources
		}
		info, e := consumer.Info(ctx)
		if e != nil {
			return nil, ErrNATSResources
		}
		capture, e := captureExisting(info, expected)
		if e != nil && i >= 2 {
			capture, e = captureExisting(info, current[i])
			if e == nil {
				want := plan.Existing[i].Configuration
				want.AckWait = approvalconsumer.AckWaitSeconds * time.Second
				want.MaxAckPending = approvalconsumer.MaxAckPending
				want.BackOff = nil
				if configurationDigest(info.Config) != configurationDigest(want) {
					return nil, ErrNATSResources
				}
				currentCount++
				// Only the configuration digest is expected to change. Instance,
				// delivery and ACK positions must still equal the original plan.
				capture.Configuration = plan.Existing[i].Configuration
				capture.ConfigurationSHA256 = plan.Existing[i].ConfigurationSHA256
			}
		} else if e == nil && i >= 2 {
			want := info.Config
			want.AckWait = approvalconsumer.AckWaitSeconds * time.Second
			want.MaxAckPending = approvalconsumer.MaxAckPending
			want.BackOff = nil
			if !validAuthorizationConsumerConfig(want, current[i]) {
				return nil, ErrNATSResources
			}
			updates = append(updates, update{want, current[i], capture})
		}
		if e != nil || !sameExistingCapture(capture, plan.Existing[i]) {
			return nil, ErrNATSResources
		}
		actual, e := captureExisting(info, current[i])
		if e != nil {
			actual, e = captureExisting(info, legacy[i])
		}
		if e != nil {
			return nil, ErrNATSResources
		}
		observed = append(observed, actual)
	}
	if inspect {
		return json.Marshal(struct {
			Schema      string                    `json:"schema"`
			OperationID string                    `json:"operation_id"`
			PlanSHA256  string                    `json:"plan_sha256"`
			Upgraded    int                       `json:"upgraded"`
			Remaining   int                       `json:"remaining"`
			Consumers   []existingConsumerCapture `json:"consumers"`
		}{"approval-token-consumer-upgrade-inspection-v1", plan.OperationID, files.PlanSHA256, currentCount, len(updates), observed})
	}
	if (len(updates) != 0 && !apply) || (apply && currentCount != 0 && !resume) {
		return nil, ErrNATSResources
	}
	before, err := stream.Info(ctx)
	if err != nil {
		return nil, ErrNATSResources
	}
	for _, item := range updates {
		// The server's response can be unknown. Resolve the original durable by
		// INFO; do not retry UPDATE or construct a replacement consumer.
		_, _ = stream.UpdateConsumer(ctx, item.configuration)
		consumer, e := stream.Consumer(ctx, item.configuration.Name)
		if e != nil {
			return nil, ErrNATSResources
		}
		info, e := consumer.Info(ctx)
		if e != nil {
			return nil, ErrNATSResources
		}
		capture, e := captureExisting(info, item.expected)
		originalConfiguration := item.original.Configuration
		expectedConfiguration := originalConfiguration
		expectedConfiguration.AckWait = approvalconsumer.AckWaitSeconds * time.Second
		expectedConfiguration.MaxAckPending = approvalconsumer.MaxAckPending
		expectedConfiguration.BackOff = nil
		if configurationDigest(info.Config) != configurationDigest(expectedConfiguration) {
			return nil, ErrNATSResources
		}
		capture.Configuration = originalConfiguration
		capture.ConfigurationSHA256 = item.original.ConfigurationSHA256
		if e != nil || !sameExistingCapture(capture, item.original) {
			return nil, ErrNATSResources
		}
	}
	after, err := stream.Info(ctx)
	if err != nil || before.Created != after.Created || before.State.Msgs != after.State.Msgs || before.State.Bytes != after.State.Bytes || before.State.FirstSeq != after.State.FirstSeq || before.State.LastSeq != after.State.LastSeq || !planStreamMatches(ctx, stream, plan) || exactConsumerNames(ctx, stream, existingNames(), existingNames()) != nil {
		return nil, ErrNATSResources
	}
	captures, err := readExistingCaptures(ctx, stream, current)
	if err != nil {
		return nil, err
	}
	for i, captured := range captures {
		want := plan.Existing[i].Configuration
		if i >= 2 {
			want.AckWait = approvalconsumer.AckWaitSeconds * time.Second
			want.MaxAckPending = approvalconsumer.MaxAckPending
			want.BackOff = nil
		}
		if configurationDigest(captured.Configuration) != configurationDigest(want) {
			return nil, ErrNATSResources
		}
		captured.Configuration = plan.Existing[i].Configuration
		captured.ConfigurationSHA256 = plan.Existing[i].ConfigurationSHA256
		if !sameExistingCapture(captured, plan.Existing[i]) {
			return nil, ErrNATSResources
		}
	}
	return json.Marshal(struct {
		Schema                                string                    `json:"schema"`
		PlanSHA256                            string                    `json:"plan_sha256"`
		OperationID                           string                    `json:"operation_id"`
		InstancePositionsAndMessagesPreserved bool                      `json:"instance_positions_and_messages_preserved"`
		Current                               []existingConsumerCapture `json:"current"`
	}{"approval-token-consumer-upgrade-result-v1", files.PlanSHA256, plan.OperationID, true, captures})
}
