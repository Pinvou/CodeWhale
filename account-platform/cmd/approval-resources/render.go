package main

import (
	"errors"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
)

// renderPublicResources is offline. Neither emitting nor hashing artifacts
// makes a digest independently trusted; operator review supplies that trust.
func renderPublicResources(environment func(string) string) (map[string]string, error) {
	identity := events.NATSAuthorizationArtifactFiles{CurrentFile: environment("APPROVAL_RESOURCE_CURRENT_PUBLIC_KEYS_FILE"), CurrentSHA256: environment("APPROVAL_RESOURCE_CURRENT_PUBLIC_KEYS_SHA256"), PreviousFile: environment("APPROVAL_RESOURCE_PREVIOUS_PUBLIC_KEYS_FILE"), PreviousSHA256: environment("APPROVAL_RESOURCE_PREVIOUS_PUBLIC_KEYS_SHA256"), PrivateIdentityFiles: []string{environment("NATS_NKEY_SEED_FILE"), environment("NATS_CREDENTIALS_FILE")}}
	if identity.Validate(time.Now().UTC()) != nil {
		return nil, errors.New("PUBLIC_RESOURCE_RENDER_REJECTED")
	}
	manifestPath := environment("APPROVAL_RESOURCE_MANIFEST_FILE")
	configurationPath := environment("APPROVAL_RESOURCE_CONFIGURATION_FILE")
	if manifestPath == configurationPath || validateNewResourcePath(manifestPath, identity) != nil || validateNewResourcePath(configurationPath, identity) != nil {
		return nil, errors.New("PUBLIC_RESOURCE_OUTPUT_REJECTED")
	}
	manifest, configuration, err := identity.ConsumerAuthorizationArtifacts(time.Now().UTC())
	if err != nil {
		return nil, errors.New("PUBLIC_RESOURCE_RENDER_REJECTED")
	}
	if writePublicReceipt(manifestPath, manifest) != nil || writePublicReceipt(configurationPath, configuration.Configuration) != nil {
		return nil, errors.New("PUBLIC_RESOURCE_WRITE_FAILED")
	}
	return map[string]string{"identity_sha256": identity.CurrentSHA256, "manifest_sha256": configuration.ManifestSHA256, "configuration_sha256": configuration.ConfigurationSHA256}, nil
}
