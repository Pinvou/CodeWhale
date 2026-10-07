package events

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/nats-io/nats.go/jetstream"
)

// FreshApprovalControl is captured by the Account control owner while its
// exclusive database row lock is retained around the broker operation.
type FreshApprovalControl struct {
	Version       int64
	ApprovalEpoch int64
	RelayEpoch    int64
}

// InitializeFreshApprovalConsumers never changes an existing resource. A
// missing publish ACK is handled by exact INFO readback, not UPDATE or DELETE.
// Existing legacy/foreign consumers and any retained authorization messages
// block this first-install path; historical cutover is intentionally separate.
func InitializeFreshApprovalConsumers(ctx context.Context, rawURL string, cfg NATSConfig, identity NATSAuthorizationArtifactFiles, manifestFile, manifestSHA string, facts FreshApprovalControl) ([]byte, error) {
	if facts.Version <= 0 || facts.ApprovalEpoch <= 0 || facts.RelayEpoch <= 0 || identity.Validate(time.Now().UTC()) != nil {
		return nil, ErrNATSResources
	}
	current, _, err := readNATSPublicArtifact(identity.CurrentFile, identity.CurrentSHA256)
	if err != nil {
		return nil, ErrNATSResources
	}
	parsed, err := parseNATSIdentityArtifact(current)
	if err != nil || parsed.version != 2 || !isolatedNATSPublicArtifactPath(manifestFile, identity.PrivateIdentityFiles) {
		return nil, ErrNATSResources
	}
	manifest, _, err := readNATSPublicArtifact(manifestFile, manifestSHA)
	if err != nil {
		return nil, ErrNATSResources
	}
	expected, err := ApprovalConsumerManifest(identity.CurrentSHA256)
	if err != nil || string(manifest) != string(expected) {
		return nil, ErrNATSResources
	}
	cfg.RequireExplicitAuthentication = true
	cfg.ResourceMode = "provisioned"
	connection, js, err := connectNATS(rawURL, "event-resource-operator", cfg)
	if err != nil {
		return nil, ErrNATSResources
	}
	defer connection.Close()
	if checkedStream(ctx, js) != nil {
		return nil, ErrNATSResources
	}
	stream, err := js.Stream(ctx, platformStream)
	if err != nil {
		return nil, ErrNATSResources
	}
	info, err := stream.Info(ctx, jetstream.WithSubjectFilter(approvalconsumer.AuthorizationSubject))
	if err != nil || info.State.Subjects[approvalconsumer.AuthorizationSubject] != 0 || info.State.LastSeq == math.MaxUint64 {
		return nil, ErrNATSResources
	}
	q := info.State.LastSeq
	deadLetters, err := stream.Info(ctx, jetstream.WithSubjectFilter("account-platform.dead-letter."+approvalconsumer.AuthorizationSubject))
	if err != nil || deadLetters.State.Subjects["account-platform.dead-letter."+approvalconsumer.AuthorizationSubject] != 0 {
		return nil, ErrNATSResources
	}
	allowed := map[string]bool{"billing-refund-executor-v1": true}
	for _, d := range approvalconsumer.BillingCommands() {
		allowed[d.Durable] = true
	}
	names := stream.ConsumerNames(ctx)
	count := 0
	for name := range names.Name() {
		count++
		if count > 4 || !allowed[name] {
			return nil, ErrNATSResources
		}
	}
	if names.Err() != nil {
		return nil, ErrNATSResources
	}
	if ProvisionNATSResources(ctx, js, true) != nil {
		return nil, ErrNATSResources
	}
	for _, descriptor := range approvalconsumer.BillingCommands() {
		configuration, err := authorizationConsumerConfig(descriptor, q+1)
		if err != nil {
			return nil, ErrNATSResources
		}
		consumer, err := stream.Consumer(ctx, descriptor.Durable)
		if errors.Is(err, jetstream.ErrConsumerNotFound) {
			_, _ = stream.CreateConsumer(ctx, configuration)
			consumer, err = stream.Consumer(ctx, descriptor.Durable)
		}
		if err != nil || !validAuthorizationConsumerConfig(consumer.CachedInfo().Config, configuration) || consumer.CachedInfo().NumPending != 0 || consumer.CachedInfo().NumAckPending != 0 {
			return nil, ErrNATSResources
		}
	}
	receipt := approvalConsumerCutoverReceipt{Schema: "approval-consumer-cutover-receipt-v1", SchemaVersion: 1, IdentitySHA256: identity.CurrentSHA256, ManifestSHA256: manifestSHA, Stream: platformStream, Control: "ADMIN_APPROVAL", CapturedMode: "QUIESCING", CapturedRelayState: "FENCED", ControlVersion: strconv.FormatInt(facts.Version, 10), ApprovalEpoch: strconv.FormatInt(facts.ApprovalEpoch, 10), AuthorizationRelayEpoch: strconv.FormatInt(facts.RelayEpoch, 10), CapturedAt: time.Now().UTC().Format(time.RFC3339Nano), HighWatermark: strconv.FormatUint(q, 10)}
	for _, d := range approvalconsumer.BillingCommands() {
		receipt.BillingStarts = append(receipt.BillingStarts, approvalConsumerReceiptStart{Durable: d.Durable, StartSequence: strconv.FormatUint(q+1, 10)})
	}
	return json.Marshal(receipt)
}

// CheckFreshApprovalConsumers re-reads every installed resource against an
// independently trusted receipt. It grants no execution or mutating ACL.
func CheckFreshApprovalConsumers(ctx context.Context, rawURL string, cfg NATSConfig, files ApprovalConsumerResourceFiles) (FreshApprovalControl, error) {
	binding, err := files.Load(time.Now().UTC())
	if err != nil {
		return FreshApprovalControl{}, ErrNATSResources
	}
	raw, _, err := readNATSPublicArtifact(files.ReceiptFile, files.ReceiptSHA256)
	if err != nil {
		return FreshApprovalControl{}, ErrNATSResources
	}
	var receipt approvalConsumerCutoverReceipt
	if json.Unmarshal(raw, &receipt) != nil {
		return FreshApprovalControl{}, ErrNATSResources
	}
	version, _ := strconv.ParseInt(receipt.ControlVersion, 10, 64)
	epoch, _ := strconv.ParseInt(receipt.ApprovalEpoch, 10, 64)
	relayEpoch, _ := strconv.ParseInt(receipt.AuthorizationRelayEpoch, 10, 64)
	cfg.RequireExplicitAuthentication = true
	cfg.ResourceMode = "provisioned"
	connection, js, err := connectNATS(rawURL, "event-resource-operator", cfg)
	if err != nil {
		return FreshApprovalControl{}, ErrNATSResources
	}
	defer connection.Close()
	if checkedStream(ctx, js) != nil {
		return FreshApprovalControl{}, ErrNATSResources
	}
	stream, err := js.Stream(ctx, platformStream)
	if err != nil {
		return FreshApprovalControl{}, ErrNATSResources
	}
	for _, subject := range []string{approvalconsumer.AuthorizationSubject, "account-platform.dead-letter." + approvalconsumer.AuthorizationSubject} {
		info, e := stream.Info(ctx, jetstream.WithSubjectFilter(subject))
		if e != nil || info.State.Subjects[subject] != 0 {
			return FreshApprovalControl{}, ErrNATSResources
		}
	}
	names := stream.ConsumerNames(ctx)
	allowed := map[string]bool{"billing-refund-executor-v1": true}
	for _, d := range approvalconsumer.BillingCommands() {
		allowed[d.Durable] = true
	}
	for name := range names.Name() {
		if !allowed[name] {
			return FreshApprovalControl{}, ErrNATSResources
		}
	}
	if names.Err() != nil {
		return FreshApprovalControl{}, ErrNATSResources
	}
	for _, d := range approvalconsumer.BillingCommands() {
		consumer, e := stream.Consumer(ctx, d.Durable)
		expected, e2 := authorizationConsumerConfig(d, binding.startSequences[d.Durable])
		if e != nil || e2 != nil || !validAuthorizationConsumerConfig(consumer.CachedInfo().Config, expected) || consumer.CachedInfo().NumPending != 0 || consumer.CachedInfo().NumAckPending != 0 {
			return FreshApprovalControl{}, ErrNATSResources
		}
	}
	return FreshApprovalControl{Version: version, ApprovalEpoch: epoch, RelayEpoch: relayEpoch}, nil
}
