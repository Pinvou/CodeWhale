package events

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/nats-io/nats.go/jetstream"
)

const platformStream = "ACCOUNT_PLATFORM"

var ErrNATSResources = errors.New("NATS_PROVISIONED_RESOURCES_UNAVAILABLE_OR_MISMATCHED")

// NATSConfig is supplied by the process boundary. Production cannot select
// auto provisioning; an omitted mode resolves according to Environment.
type NATSConfig struct {
	Environment     string
	ResourceMode    string
	CAFile          string
	SeedFile        string
	CredentialsFile string
	// RequireExplicitAuthentication forbids falling back to the process-wide
	// generic relay identity. It is used by the dedicated authorization relay.
	RequireExplicitAuthentication bool
	// ApprovalConsumerResources is a closed, verified resource/receipt binding.
	// A nil binding preserves all generic broker behavior and cannot start a
	// Q+1 Billing authorization consumer.
	ApprovalConsumerResources *ApprovalConsumerResourceBinding
}

// ApprovalConsumerResourceBinding can only be populated by the closed
// manifest/receipt reader in this package. The maps are intentionally private
// so callers cannot inject an arbitrary start sequence.
type ApprovalConsumerResourceBinding struct {
	startSequences map[string]uint64
}

func (c NATSConfig) provisioned() (bool, error) {
	production := strings.HasPrefix(strings.ToLower(strings.TrimSpace(c.Environment)), "production")
	switch c.ResourceMode {
	case "":
		return production, nil
	case "provisioned":
		return true, nil
	case "auto":
		if !production {
			return false, nil
		}
	}
	return false, ErrNATSResources
}

func platformStreamConfig() jetstream.StreamConfig {
	return jetstream.StreamConfig{
		Name: platformStream, Description: "Pinvou account platform domain events",
		Subjects: []string{"account.>", "billing.>", "token.>", "account-platform.dead-letter.>"},
		Storage:  jetstream.FileStorage, Retention: jetstream.LimitsPolicy,
		MaxAge: 30 * 24 * time.Hour, Duplicates: 24 * time.Hour,
	}
}

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
	if !ok || descriptor.Workload != broker.clientName {
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
	// Existing Token and shared Billing consumers are accepted only as an
	// explicitly transitional resource shape. This keeps the new binary usable
	// before GSA-38 performs the in-place no-BackOff/MaxDeliver convergence;
	// it is not final resource readiness.
	if descriptor.StartSource == approvalconsumer.StartExisting || descriptor.StartSource == approvalconsumer.StartLegacySharedPreFenceCompatibility {
		retained := descriptor.StartSource == approvalconsumer.StartExisting
		if validConsumerConfig(actual, eventConsumerConfig(durable, approvalconsumer.AuthorizationSubject, retained)) {
			return consumer, canDeadLetter, nil
		}
	}
	return nil, false, ErrNATSResources
}

func provisionedConsumers() []jetstream.ConsumerConfig {
	const approval = "account.approval-command.authorized.v1"
	return []jetstream.ConsumerConfig{
		eventConsumerConfig("billing-command-executor-v1", approval, false),
		eventConsumerConfig("billing-refund-executor-v1", "billing.refund.requested.v1", false),
		eventConsumerConfig("token-model-lifecycle-executor-v1", approval, true),
		eventConsumerConfig("token-catalog-rollback-executor-v1", approval, true),
		eventConsumerConfig("token-credential-revoke-executor-v1", approval, true),
	}
}

func consumerOwnedBy(role string, requested jetstream.ConsumerConfig) bool {
	for index, expected := range provisionedConsumers() {
		owner := "token-command-executor"
		if index < 2 {
			owner = "billing-command-executor"
		}
		if role == owner && reflect.DeepEqual(requested, expected) {
			return true
		}
	}
	return false
}

// ManageNATSResources connects only for an explicit, bounded operator action.
func ManageNATSResources(ctx context.Context, rawURL string, cfg NATSConfig, createOnly bool) error {
	if cfg.ResourceMode != "" && cfg.ResourceMode != "provisioned" {
		return ErrNATSResources
	}
	cfg.ResourceMode = "provisioned"
	connection, js, err := connectNATS(rawURL, "event-resource-operator", cfg)
	if err != nil {
		return err
	}
	defer connection.Close()
	return ProvisionNATSResources(ctx, js, createOnly)
}

// Normalize only documented server defaults. Every other field participates
// in equality, including security-sensitive zero values and future fields.
func validStreamConfig(actual jetstream.StreamConfig) bool {
	expected := platformStreamConfig()
	actual.Metadata = withoutNATSServerMetadata(actual.Metadata)
	actual.Subjects = slices.Clone(actual.Subjects)
	slices.Sort(actual.Subjects)
	slices.Sort(expected.Subjects)
	if actual.MaxConsumers == -1 {
		actual.MaxConsumers = 0
	}
	if actual.MaxMsgs == -1 {
		actual.MaxMsgs = 0
	}
	if actual.MaxBytes == -1 {
		actual.MaxBytes = 0
	}
	if actual.MaxMsgsPerSubject == -1 {
		actual.MaxMsgsPerSubject = 0
	}
	if actual.MaxMsgSize == -1 {
		actual.MaxMsgSize = 0
	}
	if actual.Replicas == 1 {
		actual.Replicas = 0
	}
	return reflect.DeepEqual(actual, expected)
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

func withoutNATSServerMetadata(metadata map[string]string) map[string]string {
	copy := maps.Clone(metadata)
	for _, key := range []string{"_nats.level", "_nats.req.level", "_nats.ver"} {
		delete(copy, key)
	}
	if len(copy) == 0 {
		return nil
	}
	return copy
}

func checkedStream(ctx context.Context, js jetstream.JetStream) error {
	stream, err := js.Stream(ctx, platformStream)
	if err != nil || !validStreamConfig(stream.CachedInfo().Config) {
		return ErrNATSResources
	}
	return nil
}

func checkedConsumer(ctx context.Context, js jetstream.JetStream, expected jetstream.ConsumerConfig) (jetstream.Consumer, error) {
	consumer, err := js.Consumer(ctx, platformStream, expected.Name)
	if err != nil || !validConsumerConfig(consumer.CachedInfo().Config, expected) {
		return nil, ErrNATSResources
	}
	return consumer, nil
}

// ProvisionNATSResources is a bounded operator action, not a business startup
// path. Missing resources may be created only when explicitly requested.
// Existing or concurrently created resources are never updated or deleted.
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
