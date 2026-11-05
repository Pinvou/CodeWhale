package events

import (
	"bytes"
	"encoding/json"
	"slices"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/nats-io/nkeys"
)

// ExistingBrokerAuthorizationArtifacts is a separate opt-in projection. It
// adds only INFO for the retained, unused shared Billing durable. No runtime
// receives its NEXT/ACK or any CREATE/UPDATE capability. Existing-broker
// operators never create a stream or modify the retained refund durable, so
// those inherited creation subjects are removed while their INFO remains.
func (files NATSAuthorizationArtifactFiles) ExistingBrokerAuthorizationArtifacts(now time.Time) ([]byte, NATSConsumerAuthorizationRender, error) {
	return files.existingBrokerAuthorizationArtifacts(now, false)
}

// RefundUpgradeAuthorizationArtifacts is a temporary operator-only projection.
func (files NATSAuthorizationArtifactFiles) RefundUpgradeAuthorizationArtifacts(now time.Time) ([]byte, NATSConsumerAuthorizationRender, error) {
	return files.existingBrokerAuthorizationArtifacts(now, true)
}

func (files NATSAuthorizationArtifactFiles) existingBrokerAuthorizationArtifacts(now time.Time, refundUpgrade bool) ([]byte, NATSConsumerAuthorizationRender, error) {
	manifest, rendered, err := files.ConsumerAuthorizationArtifacts(now)
	if err != nil {
		return nil, rendered, err
	}
	raw, _, err := readNATSPublicArtifact(files.CurrentFile, files.CurrentSHA256)
	if err != nil {
		return nil, rendered, err
	}
	identity, err := parseNATSIdentityArtifact(raw)
	if err != nil {
		return nil, rendered, err
	}
	var configuration struct {
		Authorization struct {
			Users []struct {
				NKey        string          `json:"nkey"`
				Permissions json.RawMessage `json:"permissions"`
			} `json:"users"`
		} `json:"authorization"`
	}
	if json.Unmarshal(rendered.Configuration, &configuration) != nil {
		return nil, rendered, ErrNATSResources
	}
	count := 0
	for i := range configuration.Authorization.Users {
		user := &configuration.Authorization.Users[i]
		if user.NKey != identity.keys["event-resource-operator"] {
			continue
		}
		count++
		var permissions natsPermissions
		if json.Unmarshal(user.Permissions, &permissions) != nil {
			return nil, rendered, ErrNATSResources
		}
		refund := provisionedConsumers()[0]
		remove := map[string]int{
			"$JS.API.STREAM.CREATE." + platformStream:                                                    0,
			"$JS.API.CONSUMER.CREATE." + platformStream + "." + refund.Name + "." + refund.FilterSubject: 0,
		}
		permissions.Publish = slices.DeleteFunc(permissions.Publish, func(value string) bool {
			if _, ok := remove[value]; ok {
				remove[value]++
				return true
			}
			return false
		})
		for _, occurrences := range remove {
			if occurrences != 1 {
				return nil, rendered, ErrNATSResources
			}
		}
		permissions.Publish = append(permissions.Publish, "$JS.API.CONSUMER.INFO."+platformStream+"."+approvalconsumer.LegacyBilling().Durable)
		if refundUpgrade {
			permissions.Publish = append(permissions.Publish, "$JS.API.CONSUMER.CREATE."+platformStream+"."+refund.Name+"."+refund.FilterSubject)
		}
		var encoded bytes.Buffer
		encoder := json.NewEncoder(&encoded)
		encoder.SetEscapeHTML(false)
		if encoder.Encode(permissions) != nil {
			return nil, rendered, ErrNATSResources
		}
		user.Permissions = bytes.TrimSpace(encoded.Bytes())
	}
	if count != 1 {
		return nil, rendered, ErrNATSResources
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(configuration) != nil {
		return nil, rendered, ErrNATSResources
	}
	rendered.Configuration = output.Bytes()
	rendered.ConfigurationSHA256 = publicSHA256(rendered.Configuration)
	return manifest, rendered, nil
}

func existingOperatorMatches(cfg NATSConfig, files NATSAuthorizationArtifactFiles) bool {
	raw, _, err := readNATSPublicArtifact(files.CurrentFile, files.CurrentSHA256)
	if err != nil {
		return false
	}
	identity, err := parseNATSIdentityArtifact(raw)
	if err != nil || identity.version != 2 {
		return false
	}
	var pair nkeys.KeyPair
	if cfg.SeedFile != "" && cfg.CredentialsFile == "" {
		pair, err = readNATSUserSeed(cfg.SeedFile)
	} else if cfg.CredentialsFile != "" && cfg.SeedFile == "" {
		_, pair, err = readNATSUserCredentials(cfg.CredentialsFile)
	} else {
		return false
	}
	if err != nil {
		return false
	}
	defer pair.Wipe()
	actual, err := pair.PublicKey()
	return err == nil && actual == identity.keys["event-resource-operator"]
}
