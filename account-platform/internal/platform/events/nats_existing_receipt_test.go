package events

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func existingReceiptFixture(t *testing.T) (ApprovalConsumerResourceFiles, approvalConsumerCutoverReceipt) {
	t.Helper()
	now := time.Now().UTC()
	files, receipt := consumerReceiptFixture(t, now, 9007199254740993)
	plan := existingBrokerPlan{Schema: existingBrokerPlanSchema, OperationID: "ba30e371-600f-47eb-b352-69ce08931341", IdentitySHA256: files.Identity.CurrentSHA256, ManifestSHA256: files.ManifestSHA256,
		StreamCreatedAt: now.Add(-time.Hour).Format(time.RFC3339Nano), ControlVersion: receipt.ControlVersion, ApprovalEpoch: receipt.ApprovalEpoch, RelayEpoch: receipt.AuthorizationRelayEpoch,
		CapturedAt: receipt.CapturedAt, HighWatermark: receipt.HighWatermark, StreamFirst: "9007199254740994", StreamMessages: "0", StreamBytes: "0"}
	for _, config := range existingBrokerConfigurations() {
		if len(config.BackOff) != 0 {
			config.AckWait = config.BackOff[0]
			config.MaxAckPending = 1000
		}
		plan.Existing = append(plan.Existing, existingConsumerCapture{Durable: config.Name, CreatedAt: now.Add(-time.Minute).Format(time.RFC3339Nano), ConfigurationSHA256: configurationDigest(config), Configuration: config, DeliveredConsumer: "0", DeliveredStream: "0", AckConsumer: "0", AckStream: "0"})
	}
	raw, _ := json.Marshal(plan)
	receipt.Schema = existingBrokerReceiptSchema
	receipt.ExistingBroker = &existingBrokerReceiptFacts{PlanSHA256: publicSHA256(raw), Plan: plan}
	replaceConsumerReceiptFixture(t, &files, receipt)
	return files, receipt
}

func TestExistingBrokerReceiptDistinctOriginAndExactPositions(t *testing.T) {
	files, receipt := existingReceiptFixture(t)
	binding, err := files.Load(time.Now().UTC())
	if err != nil || len(binding.startSequences) != 11 {
		t.Fatal("real reader rejected the distinct trusted origin")
	}
	for _, sequence := range binding.startSequences {
		if strconv.FormatUint(sequence, 10) != "9007199254740994" {
			t.Fatal("Q+1 lost integer precision")
		}
	}
	if _, err := files.VerifiedReceipt(time.Now().UTC()); err != nil {
		t.Fatal("verified bytes unavailable")
	}
	receipt.Schema = "approval-consumer-cutover-receipt-v1"
	replaceConsumerReceiptFixture(t, &files, receipt)
	if _, err := files.Load(time.Now().UTC()); err == nil {
		t.Fatal("existing-broker provenance impersonated original fresh origin")
	}
}

func TestExistingBrokerReceiptRejectsPlanDrift(t *testing.T) {
	changes := map[string]func(*existingBrokerPlan){
		"operation":            func(p *existingBrokerPlan) { p.OperationID = "00000000-0000-0000-0000-000000000000" },
		"token-upgrade-plan":   func(p *existingBrokerPlan) { p.Schema = tokenUpgradePlanSchema },
		"identity":             func(p *existingBrokerPlan) { p.IdentitySHA256 = strings.Repeat("0", 64) },
		"control":              func(p *existingBrokerPlan) { p.ControlVersion = "4" },
		"replacement-Q":        func(p *existingBrokerPlan) { p.HighWatermark = "9007199254740994" },
		"invalid-stream-size":  func(p *existingBrokerPlan) { p.StreamBytes = "1" },
		"invalid-stream-first": func(p *existingBrokerPlan) { p.StreamFirst = "9007199254740995" },
		"future":               func(p *existingBrokerPlan) { p.CapturedAt = time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano) },
		"extra-resource":       func(p *existingBrokerPlan) { p.Existing = append(p.Existing, p.Existing[0]) },
		"resource-order":       func(p *existingBrokerPlan) { p.Existing[0], p.Existing[1] = p.Existing[1], p.Existing[0] },
		"legacy-config-drift":  func(p *existingBrokerPlan) { p.Existing[0].ConfigurationSHA256 = strings.Repeat("0", 64) },
		"ack-ahead":            func(p *existingBrokerPlan) { p.Existing[0].AckConsumer = "1" },
		"position-precision":   func(p *existingBrokerPlan) { p.Existing[0].DeliveredStream = "01" },
		"position-after-Q":     func(p *existingBrokerPlan) { p.Existing[0].DeliveredStream = "9007199254740994" },
		"recreated-before-stream": func(p *existingBrokerPlan) {
			p.Existing[0].CreatedAt = time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339Nano)
		},
	}
	for name, mutate := range changes {
		t.Run(name, func(t *testing.T) {
			files, receipt := existingReceiptFixture(t)
			mutate(&receipt.ExistingBroker.Plan)
			// Reapprove both outer hashes: rejection must come from the closed
			// contract, not accidentally from an unchanged checksum.
			raw, _ := json.Marshal(receipt.ExistingBroker.Plan)
			receipt.ExistingBroker.PlanSHA256 = publicSHA256(raw)
			replaceConsumerReceiptFixture(t, &files, receipt)
			if _, err := files.Load(time.Now().UTC()); err == nil {
				t.Fatal("plan drift accepted")
			}
		})
	}
}

func TestExistingBrokerPlanRejectsNoncanonicalJSON(t *testing.T) {
	_, receipt := existingReceiptFixture(t)
	raw, _ := json.Marshal(receipt.ExistingBroker.Plan)
	for _, changed := range [][]byte{append(raw, '\n'), append([]byte(" "), raw...), []byte(strings.Replace(string(raw), `"operation_id":`, `"unknown":1,"operation_id":`, 1))} {
		if _, err := decodeExistingBrokerPlan(changed, publicSHA256(changed), time.Now().UTC()); err == nil {
			t.Fatal("noncanonical or unknown plan field accepted")
		}
	}
}

func TestExistingBrokerAuthorizationKeepsLegacyReadOnly(t *testing.T) {
	files, _ := existingReceiptFixture(t)
	manifest, rendered, err := files.Identity.ExistingBrokerAuthorizationArtifacts(time.Now().UTC())
	if err != nil || !matchesSHA256(manifest, files.ManifestSHA256) {
		t.Fatal("existing operator rendering failed")
	}
	current, _ := os.ReadFile(files.Identity.CurrentFile)
	identity, _ := parseNATSIdentityArtifact(current)
	var config struct {
		Authorization struct {
			Users []struct {
				NKey        string          `json:"nkey"`
				Permissions json.RawMessage `json:"permissions"`
			} `json:"users"`
		} `json:"authorization"`
	}
	if json.Unmarshal(rendered.Configuration, &config) != nil {
		t.Fatal("render invalid")
	}
	for _, user := range config.Authorization.Users {
		if user.NKey != identity.keys["event-resource-operator"] {
			continue
		}
		var permissions natsPermissions
		if json.Unmarshal(user.Permissions, &permissions) != nil {
			t.Fatal("operator ACL invalid")
		}
		found := false
		for _, subject := range permissions.Publish {
			if subject == "$JS.API.CONSUMER.INFO.ACCOUNT_PLATFORM.billing-command-executor-v1" {
				found = true
				continue
			}
			if strings.Contains(subject, "billing-command-executor-v1") || strings.Contains(subject, ".DELETE.") || strings.Contains(subject, ".PURGE.") || subject == "$JS.API.>" || subject == "$JS.API.STREAM.CREATE.ACCOUNT_PLATFORM" || subject == "$JS.API.CONSUMER.CREATE.ACCOUNT_PLATFORM.billing-refund-executor-v1.billing.refund.requested.v1" {
				t.Fatal("operator got legacy execution or broad mutation authority")
			}
		}
		if !found {
			t.Fatal("legacy INFO missing")
		}
	}
}
