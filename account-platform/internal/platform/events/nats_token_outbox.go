package events

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

type TokenOutboxAuthorizationKind string

const (
	TokenOutboxAuthorizationFresh          TokenOutboxAuthorizationKind = "fresh"
	TokenOutboxAuthorizationExistingBroker TokenOutboxAuthorizationKind = "existing-broker"
	TokenOutboxAuthorizationRefundUpgrade  TokenOutboxAuthorizationKind = "refund-upgrade"
)

// TokenOutboxPublicKey binds the relay to its own independently trusted key.
// It does not attest that the broker has installed the corresponding ACL.
func (files NATSAuthorizationArtifactFiles) TokenOutboxPublicKey(now time.Time) (string, error) {
	if files.Validate(now) != nil {
		return "", ErrNATSResources
	}
	current, _, err := readNATSPublicArtifact(files.CurrentFile, files.CurrentSHA256)
	if err != nil {
		return "", ErrNATSResources
	}
	primary, err := parseNATSIdentityArtifact(current)
	if err != nil || primary.version != 2 {
		return "", ErrNATSResources
	}
	return primary.keys["token-outbox-relay"], nil
}

// TokenOutboxAuthorizationArtifacts explicitly adds the previously reserved
// Token publisher to the selected, unchanged authorization carrier. It never
// replaces command-consumer permissions or accepts caller-selected subjects.
// Legacy renderers keep their original output and do not issue this identity.
func (files NATSAuthorizationArtifactFiles) TokenOutboxAuthorizationArtifacts(now time.Time, kind TokenOutboxAuthorizationKind) ([]byte, NATSConsumerAuthorizationRender, error) {
	var manifest []byte
	var configuration NATSConsumerAuthorizationRender
	var err error
	switch kind {
	case TokenOutboxAuthorizationFresh:
		manifest, configuration, err = files.ConsumerAuthorizationArtifacts(now)
	case TokenOutboxAuthorizationExistingBroker:
		manifest, configuration, err = files.ExistingBrokerAuthorizationArtifacts(now)
	case TokenOutboxAuthorizationRefundUpgrade:
		manifest, configuration, err = files.RefundUpgradeAuthorizationArtifacts(now)
	default:
		return nil, configuration, ErrNATSResources
	}
	if err != nil {
		return nil, NATSConsumerAuthorizationRender{}, err
	}
	current, _, err := readNATSPublicArtifact(files.CurrentFile, files.CurrentSHA256)
	if err != nil || configuration.IdentitySHA256 != files.CurrentSHA256 {
		return nil, NATSConsumerAuthorizationRender{}, ErrNATSResources
	}
	primary, err := parseNATSIdentityArtifact(current)
	if err != nil || primary.version != 2 {
		return nil, NATSConsumerAuthorizationRender{}, ErrNATSResources
	}
	var rendered struct {
		Authorization struct {
			Users []struct {
				NKey        string          `json:"nkey"`
				Permissions json.RawMessage `json:"permissions"`
			} `json:"users"`
		} `json:"authorization"`
	}
	if json.Unmarshal(configuration.Configuration, &rendered) != nil {
		return nil, NATSConsumerAuthorizationRender{}, ErrNATSResources
	}
	public := primary.keys["token-outbox-relay"]
	for _, user := range rendered.Authorization.Users {
		if user.NKey == public {
			return nil, NATSConsumerAuthorizationRender{}, ErrNATSResources
		}
	}
	var permissions bytes.Buffer
	permissionEncoder := json.NewEncoder(&permissions)
	permissionEncoder.SetEscapeHTML(false)
	if permissionEncoder.Encode(fixedNATSPermissions("token-outbox-relay")) != nil {
		return nil, NATSConsumerAuthorizationRender{}, ErrNATSResources
	}
	rendered.Authorization.Users = append(rendered.Authorization.Users, struct {
		NKey        string          `json:"nkey"`
		Permissions json.RawMessage `json:"permissions"`
	}{NKey: public, Permissions: bytes.TrimSpace(permissions.Bytes())})
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(rendered) != nil {
		return nil, NATSConsumerAuthorizationRender{}, ErrNATSResources
	}
	configuration.Configuration = output.Bytes()
	digest := sha256.Sum256(configuration.Configuration)
	configuration.ConfigurationSHA256 = hex.EncodeToString(digest[:])
	return manifest, configuration, nil
}
