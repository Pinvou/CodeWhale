package events

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

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
	ApprovalConsumerResources     *ApprovalConsumerResourceBinding
	RequireExplicitAuthentication bool
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
