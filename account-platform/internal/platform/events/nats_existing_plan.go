package events

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"reflect"
	"strconv"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
)

const existingBrokerPlanSchema = "approval-existing-broker-plan-v1"
const existingBrokerReceiptSchema = "approval-existing-broker-cutover-receipt-v1"
const tokenUpgradePlanSchema = "approval-token-consumer-upgrade-plan-v1"
const refundUpgradePlanSchema = "approval-refund-consumer-upgrade-plan-v1"

// Captures are public metadata, not event payloads. Integer strings preserve
// exact sequence positions across JSON readers and operator review tools.
type existingConsumerCapture struct {
	Durable             string                   `json:"durable"`
	CreatedAt           string                   `json:"created_at"`
	ConfigurationSHA256 string                   `json:"configuration_sha256"`
	Configuration       jetstream.ConsumerConfig `json:"configuration"`
	DeliveredConsumer   string                   `json:"delivered_consumer"`
	DeliveredStream     string                   `json:"delivered_stream"`
	AckConsumer         string                   `json:"ack_consumer"`
	AckStream           string                   `json:"ack_stream"`
}

type existingBrokerPlan struct {
	Schema          string                    `json:"schema"`
	OperationID     string                    `json:"operation_id"`
	IdentitySHA256  string                    `json:"identity_sha256"`
	ManifestSHA256  string                    `json:"manifest_sha256"`
	StreamCreatedAt string                    `json:"stream_created_at"`
	ControlVersion  string                    `json:"control_version"`
	ApprovalEpoch   string                    `json:"approval_epoch"`
	RelayEpoch      string                    `json:"relay_epoch"`
	CapturedAt      string                    `json:"captured_at"`
	HighWatermark   string                    `json:"high_watermark"`
	StreamFirst     string                    `json:"stream_first"`
	StreamMessages  string                    `json:"stream_messages"`
	StreamBytes     string                    `json:"stream_bytes"`
	Existing        []existingConsumerCapture `json:"existing"`
}

type existingBrokerReceiptFacts struct {
	PlanSHA256 string             `json:"plan_sha256"`
	Plan       existingBrokerPlan `json:"plan"`
}

// These five resources are the only supported pre-existing broker shape.
// The shared Billing durable remains unused and receives no execution ACL.
func existingBrokerConfigurations() []jetstream.ConsumerConfig {
	result := []jetstream.ConsumerConfig{
		eventConsumerConfig(approvalconsumer.LegacyBilling().Durable, approvalconsumer.AuthorizationSubject, true),
		provisionedConsumers()[0],
	}
	for _, descriptor := range approvalconsumer.TokenCommands() {
		configuration, _ := authorizationConsumerConfig(descriptor, 0)
		result = append(result, configuration)
	}
	return result
}

// refundUpgradeConfigurations is the exact historical pre-upgrade state.
func refundUpgradeConfigurations() []jetstream.ConsumerConfig {
	result := tokenUpgradeConfigurations()
	result[0].MaxDeliver = 12
	result[1].MaxDeliver = 12
	return result
}

func tokenUpgradeConfigurations() []jetstream.ConsumerConfig {
	result := existingBrokerConfigurations()
	for i := 2; i < len(result); i++ {
		result[i] = eventConsumerConfig(result[i].Name, approvalconsumer.AuthorizationSubject, true)
	}
	return result
}

func existingConfigurationValid(actual, expected jetstream.ConsumerConfig) bool {
	if expected.Name == approvalconsumer.LegacyBilling().Durable && actual.MaxDeliver == 12 {
		expected.MaxDeliver = 12
	}
	if len(expected.BackOff) != 0 {
		// The historical server projects the first BackOff into AckWait and
		// supplies MaxAckPending=1000. Do not accept alternate values here:
		// this operator upgrades only that precise published legacy shape.
		expected.AckWait = expected.BackOff[0]
		expected.MaxAckPending = 1000
	}
	return validAuthorizationConsumerConfig(actual, expected)
}

func sameExistingCapture(a, b existingConsumerCapture) bool { return reflect.DeepEqual(a, b) }

func configurationDigest(configuration jetstream.ConsumerConfig) string {
	raw, _ := json.Marshal(configuration)
	return publicSHA256(raw)
}

func publicSHA256(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func exactUnsigned(value string) (uint64, bool) {
	v, err := strconv.ParseUint(value, 10, 64)
	return v, err == nil && strconv.FormatUint(v, 10) == value
}

func exactTime(value string, now time.Time) (time.Time, bool) {
	v, err := time.Parse(time.RFC3339Nano, value)
	return v, err == nil && v.Location() == time.UTC && v.Format(time.RFC3339Nano) == value && !v.After(now)
}

func (plan existingBrokerPlan) validate(now time.Time) error {
	return plan.validateKind(now, false)
}

func (plan existingBrokerPlan) validateKind(now time.Time, tokenUpgrade bool) error {
	kind := "existing"
	if tokenUpgrade {
		kind = "token"
	}
	return plan.validateOperation(now, kind)
}

func (plan existingBrokerPlan) validateOperation(now time.Time, kind string) error {
	schema := existingBrokerPlanSchema
	wants := existingBrokerConfigurations()
	switch kind {
	case "existing":
	case "token":
		schema, wants = tokenUpgradePlanSchema, tokenUpgradeConfigurations()
	case "refund":
		schema, wants = refundUpgradePlanSchema, refundUpgradeConfigurations()
	default:
		return ErrNATSResources
	}
	id, err := uuid.Parse(plan.OperationID)
	if err != nil || id == uuid.Nil || id.String() != plan.OperationID || plan.Schema != schema || !canonicalSHA256(plan.IdentitySHA256) || !canonicalSHA256(plan.ManifestSHA256) {
		return ErrNATSResources
	}
	for _, value := range []string{plan.ControlVersion, plan.ApprovalEpoch, plan.RelayEpoch} {
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil || v <= 0 || strconv.FormatInt(v, 10) != value {
			return ErrNATSResources
		}
	}
	q, ok := exactUnsigned(plan.HighWatermark)
	if !ok || q == math.MaxUint64 {
		return ErrNATSResources
	}
	first, validFirst := exactUnsigned(plan.StreamFirst)
	messages, validMessages := exactUnsigned(plan.StreamMessages)
	byteCount, validBytes := exactUnsigned(plan.StreamBytes)
	if !validFirst || !validMessages || !validBytes || first > q+1 ||
		(messages == 0 && byteCount != 0) ||
		(messages > 0 && (first == 0 || first > q || messages > q-first+1 || byteCount == 0)) {
		return ErrNATSResources
	}
	captured, ok := exactTime(plan.CapturedAt, now)
	if !ok {
		return ErrNATSResources
	}
	created, ok := exactTime(plan.StreamCreatedAt, captured)
	if !ok {
		return ErrNATSResources
	}
	if len(plan.Existing) != len(wants) {
		return ErrNATSResources
	}
	for i, entry := range plan.Existing {
		t, ok := exactTime(entry.CreatedAt, captured)
		if !ok || t.Before(created) || entry.Durable != wants[i].Name || !existingConfigurationValid(entry.Configuration, wants[i]) || entry.ConfigurationSHA256 != configurationDigest(entry.Configuration) {
			return ErrNATSResources
		}
		dc, a := exactUnsigned(entry.DeliveredConsumer)
		ds, b := exactUnsigned(entry.DeliveredStream)
		ac, c := exactUnsigned(entry.AckConsumer)
		as, d := exactUnsigned(entry.AckStream)
		if !a || !b || !c || !d || ac > dc || as > ds || ds > q {
			return ErrNATSResources
		}
	}
	return nil
}

func decodeExistingBrokerPlan(raw []byte, expected string, now time.Time) (existingBrokerPlan, error) {
	return decodeBrokerPlan(raw, expected, now, false)
}

func decodeBrokerPlan(raw []byte, expected string, now time.Time, tokenUpgrade bool) (existingBrokerPlan, error) {
	kind := "existing"
	if tokenUpgrade {
		kind = "token"
	}
	return decodeBrokerOperation(raw, expected, now, kind)
}

func decodeBrokerOperation(raw []byte, expected string, now time.Time, kind string) (existingBrokerPlan, error) {
	var plan existingBrokerPlan
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if !matchesSHA256(raw, expected) || decoder.Decode(&plan) != nil || plan.validateOperation(now, kind) != nil {
		return plan, ErrNATSResources
	}
	canonical, err := json.Marshal(plan)
	if err != nil || !bytes.Equal(raw, canonical) {
		return plan, ErrNATSResources
	}
	return plan, nil
}

func captureExisting(info *jetstream.ConsumerInfo, expected jetstream.ConsumerConfig) (existingConsumerCapture, error) {
	if info == nil || info.Name != expected.Name || !existingConfigurationValid(info.Config, expected) || info.NumPending != 0 || info.NumAckPending != 0 {
		return existingConsumerCapture{}, ErrNATSResources
	}
	return existingConsumerCapture{Durable: info.Name, CreatedAt: info.Created.UTC().Format(time.RFC3339Nano), ConfigurationSHA256: configurationDigest(info.Config), Configuration: info.Config,
		DeliveredConsumer: strconv.FormatUint(info.Delivered.Consumer, 10), DeliveredStream: strconv.FormatUint(info.Delivered.Stream, 10),
		AckConsumer: strconv.FormatUint(info.AckFloor.Consumer, 10), AckStream: strconv.FormatUint(info.AckFloor.Stream, 10)}, nil
}
