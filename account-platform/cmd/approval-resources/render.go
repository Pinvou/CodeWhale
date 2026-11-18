package main

import (
	"errors"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
)

// renderPublicResources is offline. Neither emitting nor hashing artifacts
// makes a digest independently trusted; operator review supplies that trust.
func renderPublicResources(environment func(string) string) (map[string]string, error) {
	return renderResources(environment, false)
}

func renderResources(environment func(string) string, existingBroker bool) (map[string]string, error) {
	return renderResourcesKind(environment, existingBroker, false)
}

func renderResourcesKind(environment func(string) string, existingBroker, refundUpgrade bool) (map[string]string, error) {
	return renderResourcesSelection(environment, existingBroker, refundUpgrade, false)
}

func renderResourcesSelection(environment func(string) string, existingBroker, refundUpgrade, tokenEvents bool) (map[string]string, error) {
	profile, err := config.ParseAccountPlatformProfile(environment("ACCOUNT_PLATFORM_PROFILE"))
	if err != nil || (tokenEvents && profile != config.AccountPlatformProfileModels) {
		return nil, errors.New("PUBLIC_RESOURCE_RENDER_REJECTED")
	}
	identity := events.NATSAuthorizationArtifactFiles{CurrentFile: environment("APPROVAL_RESOURCE_CURRENT_PUBLIC_KEYS_FILE"), CurrentSHA256: environment("APPROVAL_RESOURCE_CURRENT_PUBLIC_KEYS_SHA256"), PreviousFile: environment("APPROVAL_RESOURCE_PREVIOUS_PUBLIC_KEYS_FILE"), PreviousSHA256: environment("APPROVAL_RESOURCE_PREVIOUS_PUBLIC_KEYS_SHA256"), PrivateIdentityFiles: []string{environment("NATS_NKEY_SEED_FILE"), environment("NATS_CREDENTIALS_FILE")}}
	if identity.Validate(time.Now().UTC()) != nil {
		return nil, errors.New("PUBLIC_RESOURCE_RENDER_REJECTED")
	}
	manifestPath := environment("APPROVAL_RESOURCE_MANIFEST_FILE")
	configurationPath := environment("APPROVAL_RESOURCE_CONFIGURATION_FILE")
	if manifestPath == configurationPath || validateNewResourcePath(manifestPath, identity) != nil || validateNewResourcePath(configurationPath, identity) != nil {
		return nil, errors.New("PUBLIC_RESOURCE_OUTPUT_REJECTED")
	}
	var manifest []byte
	var configuration events.NATSConsumerAuthorizationRender
	if tokenEvents {
		kind := events.TokenOutboxAuthorizationFresh
		if existingBroker {
			kind = events.TokenOutboxAuthorizationExistingBroker
		}
		if refundUpgrade {
			kind = events.TokenOutboxAuthorizationRefundUpgrade
		}
		manifest, configuration, err = identity.TokenOutboxAuthorizationArtifacts(time.Now().UTC(), kind)
	} else {
		manifest, configuration, err = identity.ConsumerAuthorizationArtifacts(time.Now().UTC())
		if existingBroker {
			manifest, configuration, err = identity.ExistingBrokerAuthorizationArtifacts(time.Now().UTC())
		}
		if refundUpgrade {
			manifest, configuration, err = identity.RefundUpgradeAuthorizationArtifacts(time.Now().UTC())
		}
	}
	if err != nil {
		return nil, errors.New("PUBLIC_RESOURCE_RENDER_REJECTED")
	}
	if writePublicReceipt(manifestPath, manifest) != nil || writePublicReceipt(configurationPath, configuration.Configuration) != nil {
		return nil, errors.New("PUBLIC_RESOURCE_WRITE_FAILED")
	}
	return map[string]string{"identity_sha256": identity.CurrentSHA256, "manifest_sha256": configuration.ManifestSHA256, "configuration_sha256": configuration.ConfigurationSHA256}, nil
}
