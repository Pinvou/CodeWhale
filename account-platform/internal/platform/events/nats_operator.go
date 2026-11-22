package events

import (
	"context"
	"errors"

	"github.com/nats-io/nats.go/jetstream"
)

// ManageNATSResources is a bounded operator entry, never a runtime fallback.
// Only this delivery's fixed ordinary consumers may be installed. Approval
// consumers still require their independent control/receipt initialization.
func ManageNATSResources(ctx context.Context, rawURL string, cfg NATSConfig, createOnly bool) error {
	if cfg.ResourceMode != "" && cfg.ResourceMode != "provisioned" {
		return ErrNATSResources
	}
	cfg.ResourceMode = "provisioned"
	cfg.RequireExplicitAuthentication = true
	connection, js, err := connectNATS(rawURL, "event-resource-operator", cfg)
	if err != nil {
		return ErrNATSResources
	}
	defer connection.Close()
	stream, err := js.Stream(ctx, platformStream)
	if errors.Is(err, jetstream.ErrStreamNotFound) && createOnly {
		// An uncertain create is resolved by the authoritative read, never an
		// update or delete. Existing drift is preserved and fails the check.
		_, _ = js.CreateStream(ctx, platformStreamConfig())
		stream, err = js.Stream(ctx, platformStream)
	}
	if err != nil || !validStreamConfig(stream.CachedInfo().Config) {
		return ErrNATSResources
	}
	return ProvisionNATSResources(ctx, js, createOnly)
}
