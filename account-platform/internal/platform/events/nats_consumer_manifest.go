package events

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
)

type approvalConsumerManifestEntry struct {
	CommandType            string `json:"command_type"`
	PolicyVersion          int64  `json:"policy_version"`
	ExecutorIdentity       string `json:"executor_identity"`
	Durable                string `json:"durable"`
	Workload               string `json:"workload"`
	FilterSubject          string `json:"filter_subject"`
	StartSource            string `json:"start_source"`
	AckWaitSeconds         int    `json:"ack_wait_seconds"`
	MaximumNAKDelaySeconds int    `json:"maximum_nak_delay_seconds"`
	MaxAckPending          int    `json:"max_ack_pending"`
	MaxDeliver             int    `json:"max_deliver"`
}

// ApprovalConsumerManifest emits only a fixed expectation, never a captured Q,
// permission grant, activation receipt or proof that a broker is ready.
// The identity digest must come from the independently trusted v2 projection.
func ApprovalConsumerManifest(identitySHA256 string) ([]byte, error) {
	if !canonicalSHA256(identitySHA256) {
		return nil, ErrNATSResources
	}
	entries := make([]approvalConsumerManifestEntry, 0, 6)
	for _, descriptor := range approvalconsumer.All() {
		start := "existing"
		if descriptor.StartSource == approvalconsumer.StartQuiescingHighWatermarkPlusOne {
			start = "quiescing_high_watermark_plus_one"
		}
		entries = append(entries, approvalConsumerManifestEntry{
			CommandType: descriptor.CommandType, PolicyVersion: descriptor.PolicyVersion,
			ExecutorIdentity: descriptor.ExecutorIdentity, Durable: descriptor.Durable,
			Workload: descriptor.Workload, FilterSubject: authorizationSubject, StartSource: start,
			AckWaitSeconds: approvalconsumer.AckWaitSeconds, MaximumNAKDelaySeconds: 512,
			MaxAckPending: approvalconsumer.MaxAckPending, MaxDeliver: -1,
		})
	}
	return json.Marshal(struct {
		Schema         string                          `json:"schema"`
		IdentitySHA256 string                          `json:"identity_sha256"`
		Stream         string                          `json:"stream"`
		Consumers      []approvalConsumerManifestEntry `json:"consumers"`
	}{"approval-consumer-resources-v1", identitySHA256, platformStream, entries})
}

func canonicalSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value
}

func matchesSHA256(raw []byte, expected string) bool {
	actual := sha256.Sum256(raw)
	return canonicalSHA256(expected) && hex.EncodeToString(actual[:]) == expected
}

// NATSConsumerAuthorizationRender carries separate evidence alongside a valid
// NATS configuration. These hashes describe offline rendering, not runtime ACK,
// resource readiness, Q capture or activation.
type NATSConsumerAuthorizationRender struct {
	Configuration       []byte
	IdentitySHA256      string
	ManifestSHA256      string
	ConfigurationSHA256 string
}

// ConsumerAuthorizationArtifacts uses the same bounded, non-linked file
// readers as runtime validation. Output hashes still require independent trust.
func (files NATSAuthorizationArtifactFiles) ConsumerAuthorizationArtifacts(now time.Time) ([]byte, NATSConsumerAuthorizationRender, error) {
	if files.Validate(now) != nil {
		return nil, NATSConsumerAuthorizationRender{}, ErrNATSResources
	}
	current, _, err := readNATSPublicArtifact(files.CurrentFile, files.CurrentSHA256)
	if err != nil {
		return nil, NATSConsumerAuthorizationRender{}, ErrNATSResources
	}
	var previous []byte
	if files.PreviousFile != "" {
		previous, _, err = readNATSPublicArtifact(files.PreviousFile, files.PreviousSHA256)
		if err != nil {
			return nil, NATSConsumerAuthorizationRender{}, ErrNATSResources
		}
	}
	manifest, err := ApprovalConsumerManifest(files.CurrentSHA256)
	if err != nil {
		return nil, NATSConsumerAuthorizationRender{}, ErrNATSResources
	}
	digest := sha256.Sum256(manifest)
	configuration, err := renderNATSAuthorizationWithConsumersAt(current, previous, manifest, files.CurrentSHA256, hex.EncodeToString(digest[:]), now)
	return manifest, configuration, err
}

// RenderNATSAuthorizationWithConsumers is opt-in. The original v1/v2 renderer
// remains unchanged. Canonical byte equality deliberately rejects unknown or
// duplicate fields, alternative mappings and caller-selected resource settings.
func RenderNATSAuthorizationWithConsumers(current, previous, manifest []byte, identitySHA256, manifestSHA256 string) (NATSConsumerAuthorizationRender, error) {
	return renderNATSAuthorizationWithConsumersAt(current, previous, manifest, identitySHA256, manifestSHA256, time.Now().UTC().Truncate(time.Second))
}

func renderNATSAuthorizationWithConsumersAt(current, previous, manifest []byte, identitySHA256, manifestSHA256 string, now time.Time) (NATSConsumerAuthorizationRender, error) {
	var result NATSConsumerAuthorizationRender
	if !matchesSHA256(current, identitySHA256) || !matchesSHA256(manifest, manifestSHA256) {
		return result, ErrNATSResources
	}
	primary, err := parseNATSIdentityArtifact(current)
	if err != nil || primary.version != 2 {
		return result, ErrNATSResources
	}
	expected, err := ApprovalConsumerManifest(identitySHA256)
	if err != nil || !bytes.Equal(expected, manifest) {
		return result, ErrNATSResources
	}
	base, err := renderNATSAuthorizationAt(current, previous, now)
	if err != nil {
		return result, err
	}
	var rendered struct {
		Authorization struct {
			Users []struct {
				NKey        string          `json:"nkey"`
				Permissions json.RawMessage `json:"permissions"`
			} `json:"users"`
		} `json:"authorization"`
	}
	if json.Unmarshal(base, &rendered) != nil {
		return result, ErrNATSResources
	}
	// Only the opt-in, digest-bound consumer renderer first installs Token's
	// three command consumer identity. The legacy renderers and ordinary Token
	// outbox identity remain unchanged and unissued.
	permissionsRaw, err := json.Marshal(fixedNATSPermissions(approvalconsumer.TokenWorkload))
	if err != nil {
		return result, ErrNATSResources
	}
	rendered.Authorization.Users = append(rendered.Authorization.Users, struct {
		NKey        string          `json:"nkey"`
		Permissions json.RawMessage `json:"permissions"`
	}{NKey: primary.keys[approvalconsumer.TokenWorkload], Permissions: permissionsRaw})
	for index := range rendered.Authorization.Users {
		user := &rendered.Authorization.Users[index]
		for _, role := range []string{approvalconsumer.BillingWorkload, approvalconsumer.TokenWorkload, "event-resource-operator"} {
			if user.NKey != primary.keys[role] {
				continue
			}
			permissions := fixedNATSPermissions(role)
			for _, descriptor := range approvalconsumer.All() {
				if role != "event-resource-operator" && descriptor.Workload != role {
					continue
				}
				subjects := []string{
					"$JS.API.CONSUMER.INFO." + platformStream + "." + descriptor.Durable,
					"$JS.API.CONSUMER.MSG.NEXT." + platformStream + "." + descriptor.Durable,
					"$JS.ACK." + platformStream + "." + descriptor.Durable + ".>",
				}
				if role == "event-resource-operator" {
					subjects = []string{"$JS.API.CONSUMER.INFO." + platformStream + "." + descriptor.Durable,
						"$JS.API.CONSUMER.CREATE." + platformStream + "." + descriptor.Durable + "." + approvalconsumer.AuthorizationSubject}
				}
				for _, subject := range subjects {
					if !slices.Contains(permissions.Publish, subject) {
						permissions.Publish = append(permissions.Publish, subject)
					}
				}
			}
			// NATS configuration accepts JSON-shaped input but not JSON \u
			// escapes. Disable HTML escaping at this intermediate RawMessage too.
			var permissionBytes bytes.Buffer
			permissionEncoder := json.NewEncoder(&permissionBytes)
			permissionEncoder.SetEscapeHTML(false)
			if permissionEncoder.Encode(permissions) != nil {
				return result, ErrNATSResources
			}
			user.Permissions = bytes.TrimSpace(permissionBytes.Bytes())
		}
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(rendered) != nil {
		return result, ErrNATSResources
	}
	digest := sha256.Sum256(output.Bytes())
	return NATSConsumerAuthorizationRender{Configuration: output.Bytes(), IdentitySHA256: identitySHA256,
		ManifestSHA256: manifestSHA256, ConfigurationSHA256: hex.EncodeToString(digest[:])}, nil
}
