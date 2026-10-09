package events

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/nats-io/nats.go/jetstream"
	"reflect"
	"time"
)

type ApprovalConsumerResourceBinding struct{ startSequences map[string]uint64 }

func eventConsumerConfig(durable, filter string, retained bool) jetstream.ConsumerConfig {
	maxDeliver := 12
	if retained {
		maxDeliver = -1
	}
	return jetstream.ConsumerConfig{Name: durable, Durable: durable, FilterSubject: filter,
		AckPolicy: jetstream.AckExplicitPolicy, AckWait: 30 * time.Second, MaxDeliver: maxDeliver,
		BackOff: []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute}}
}

func authorizationConsumerConfig(descriptor approvalconsumer.Descriptor, startSequence uint64) (jetstream.ConsumerConfig, error) {
	registered, ok := approvalconsumer.LookupDurable(descriptor.Durable)
	if !ok || registered != descriptor {
		return jetstream.ConsumerConfig{}, ErrNATSResources
	}
	config := jetstream.ConsumerConfig{
		Name: descriptor.Durable, Durable: descriptor.Durable,
		FilterSubject: approvalconsumer.AuthorizationSubject,
		AckPolicy:     jetstream.AckExplicitPolicy, AckWait: approvalconsumer.AckWaitSeconds * time.Second,
		MaxDeliver: -1, MaxAckPending: approvalconsumer.MaxAckPending,
	}
	if descriptor.StartSource == approvalconsumer.StartQuiescingHighWatermarkPlusOne {
		// An empty stream has Q=0, so its trusted Q+1 start is exactly 1.
		if startSequence == 0 {
			return jetstream.ConsumerConfig{}, ErrNATSResources
		}
		config.DeliverPolicy = jetstream.DeliverByStartSequencePolicy
		config.OptStartSeq = startSequence
	} else if startSequence != 0 {
		return jetstream.ConsumerConfig{}, ErrNATSResources
	}
	return config, nil
}

func validAuthorizationConsumerConfig(actual, expected jetstream.ConsumerConfig) bool {
	actual.Metadata = withoutNATSServerMetadata(actual.Metadata)
	if actual.MaxWaiting == 512 {
		actual.MaxWaiting = 0
	}
	if actual.Replicas == 1 {
		actual.Replicas = 0
	}
	// Unlike validConsumerConfig, never normalize AckWait from BackOff[0]
	// and never normalize MaxAckPending. Both are frozen authorization facts.
	return reflect.DeepEqual(actual, expected)
}

func (broker *NATSBroker) authorizationConsumer(ctx context.Context, durable string) (jetstream.Consumer, bool, error) {
	if broker == nil || broker.stream == nil || broker.publishOnly || !broker.provisioned {
		return nil, false, ErrNATSResources
	}
	descriptor, ok := approvalconsumer.LookupDurable(durable)
	if !ok || descriptor.CommandType == "" || descriptor.Workload != broker.clientName {
		return nil, false, ErrNATSResources
	}
	canDeadLetter := descriptor.Workload == approvalconsumer.BillingWorkload
	startSequence := uint64(0)
	if descriptor.StartSource == approvalconsumer.StartQuiescingHighWatermarkPlusOne {
		if broker.approvalResources == nil {
			return nil, false, ErrNATSResources
		}
		startSequence = broker.approvalResources.startSequences[durable]
	}
	expected, err := authorizationConsumerConfig(descriptor, startSequence)
	if err != nil {
		return nil, false, err
	}
	consumer, err := broker.stream.Consumer(ctx, platformStream, durable)
	if err != nil {
		return nil, false, ErrNATSResources
	}
	actual := consumer.CachedInfo().Config
	if validAuthorizationConsumerConfig(actual, expected) {
		return consumer, canDeadLetter, nil
	}
	return nil, false, ErrNATSResources
}

func provisionedConsumers() []jetstream.ConsumerConfig {
	return []jetstream.ConsumerConfig{
		// A provider outage must keep the durable command for an explicit retry;
		// production uses ConsumeUntilResolved, so its provisioned config agrees.
		eventConsumerConfig("billing-refund-executor-v1", "billing.refund.requested.v1", true),
		// Full membership snapshots repair durable quota authority after every
		// member role, ownership or lease change. An outage must retain them.
		eventConsumerConfig("billing-member-lifecycle-v1", "account.membership.changed.v1", true),
	}
}

func consumerOwnedBy(role string, requested jetstream.ConsumerConfig) bool {
	for _, expected := range provisionedConsumers() {
		if role == "billing-command-executor" && reflect.DeepEqual(requested, expected) {
			return true
		}
	}
	return false
}

func validConsumerConfig(actual, expected jetstream.ConsumerConfig) bool {
	actual.Metadata = withoutNATSServerMetadata(actual.Metadata)
	if actual.AckWait == expected.BackOff[0] {
		actual.AckWait = expected.AckWait
	}
	if actual.MaxWaiting == 512 {
		actual.MaxWaiting = 0
	}
	if actual.MaxAckPending == 1000 {
		actual.MaxAckPending = 0
	}
	if actual.Replicas == 1 {
		actual.Replicas = 0
	}
	return reflect.DeepEqual(actual, expected)
}

func checkedConsumer(ctx context.Context, js jetstream.JetStream, expected jetstream.ConsumerConfig) (jetstream.Consumer, error) {
	consumer, err := js.Consumer(ctx, platformStream, expected.Name)
	if err != nil || !validConsumerConfig(consumer.CachedInfo().Config, expected) {
		return nil, ErrNATSResources
	}
	return consumer, nil
}

func ProvisionNATSResources(ctx context.Context, js jetstream.JetStream, createOnly bool) error {
	stream, err := js.Stream(ctx, platformStream)
	if errors.Is(err, jetstream.ErrStreamNotFound) && createOnly {
		_, _ = js.CreateStream(ctx, platformStreamConfig())
		stream, err = js.Stream(ctx, platformStream)
	}
	if err != nil || !validStreamConfig(stream.CachedInfo().Config) {
		return ErrNATSResources
	}
	// Inspect all existing consumers before creating any missing one, so an
	// existing drift fails without incidental consumer creations.
	missing := []jetstream.ConsumerConfig{}
	for _, expected := range provisionedConsumers() {
		consumer, err := js.Consumer(ctx, platformStream, expected.Name)
		if errors.Is(err, jetstream.ErrConsumerNotFound) && createOnly {
			missing = append(missing, expected)
			continue
		}
		if err != nil || !validConsumerConfig(consumer.CachedInfo().Config, expected) {
			return ErrNATSResources
		}
	}
	for _, expected := range missing {
		_, _ = js.CreateConsumer(ctx, platformStream, expected)
		if _, err := checkedConsumer(ctx, js, expected); err != nil {
			return err
		}
	}
	return nil
}
