package events

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/nats-io/nats.go/jetstream"
	"time"
)

// ProvisionTokenApprovalConsumers is a separately invoked operator step. It
// never updates/deletes existing resources or recreates Billing's Q+1 receipt.
// Token command consumers use their original EXISTING delivery policy so each
// raw adapter can inspect the shared historical authorization route safely.
func ProvisionTokenApprovalConsumers(ctx context.Context, rawURL string, cfg NATSConfig, identity NATSAuthorizationArtifactFiles, manifestFile, manifestSHA string, createMissing bool) ([]byte, error) {
	if identity.Validate(time.Now().UTC()) != nil || !isolatedNATSPublicArtifactPath(manifestFile, identity.PrivateIdentityFiles) {
		return nil, ErrNATSResources
	}
	manifest, _, err := readNATSPublicArtifact(manifestFile, manifestSHA)
	expected, e := ApprovalConsumerManifest(identity.CurrentSHA256)
	if err != nil || e != nil || string(manifest) != string(expected) {
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
	// Read all Token configurations before the first creation. Drift never
	// grants an UPDATE/DELETE permission or creates incidental sibling resources.
	missing := []jetstream.ConsumerConfig{}
	for _, d := range approvalconsumer.TokenCommands() {
		want, err := authorizationConsumerConfig(d, 0)
		if err != nil {
			return nil, ErrNATSResources
		}
		consumer, err := stream.Consumer(ctx, d.Durable)
		if errors.Is(err, jetstream.ErrConsumerNotFound) && createMissing {
			missing = append(missing, want)
			continue
		}
		if err != nil || !validAuthorizationConsumerConfig(consumer.CachedInfo().Config, want) {
			return nil, ErrNATSResources
		}
	}
	for _, want := range missing {
		// Unknown create ACK is resolved only by exact INFO readback.
		_, _ = stream.CreateConsumer(ctx, want)
		consumer, err := stream.Consumer(ctx, want.Durable)
		if err != nil || !validAuthorizationConsumerConfig(consumer.CachedInfo().Config, want) {
			return nil, ErrNATSResources
		}
	}
	// Final fresh readback covers all siblings, including concurrently changed
	// operator resources. The returned public evidence has no secret or payload.
	descriptors := approvalconsumer.TokenCommands()
	for _, d := range descriptors {
		consumer, err := stream.Consumer(ctx, d.Durable)
		want, e := authorizationConsumerConfig(d, 0)
		if err != nil || e != nil || !validAuthorizationConsumerConfig(consumer.CachedInfo().Config, want) {
			return nil, ErrNATSResources
		}
	}
	return json.Marshal(struct {
		Schema         string                        `json:"schema"`
		IdentitySHA256 string                        `json:"identity_sha256"`
		ManifestSHA256 string                        `json:"manifest_sha256"`
		Stream         string                        `json:"stream"`
		Consumers      []approvalconsumer.Descriptor `json:"consumers"`
		VerifiedAt     string                        `json:"verified_at"`
	}{"token-approval-consumer-provision-v1", identity.CurrentSHA256, manifestSHA, platformStream, descriptors, time.Now().UTC().Format(time.RFC3339Nano)})
}
