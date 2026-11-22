package approvalencoding

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestProductResumeDefaultProviderProof(t *testing.T) {
	t.Run("virtual-zero-survives-owner-account-and-legacy-roundtrip", func(t *testing.T) {
		input := defaultProviderResumeProofInput(t, "PAYMENT_PRODUCT_RESUME", 0)
		artifacts, err := ConvertLegacyProofInput(input)
		if err != nil {
			t.Fatal("the actual owner proof converter must accept the virtual healthy provider", err)
		}
		decoded, err := DecodeProofInput(input.CommandType, input.PolicyVersion, artifacts.ProofInputJSON)
		if err != nil || decoded.CommandDigest != artifacts.CommandDigest || decoded.SnapshotDigest != artifacts.SnapshotDigest {
			t.Fatal("the Account decoder must accept the same exact proof", err)
		}
		legacy, err := LegacyProofInputFromArtifacts(decoded)
		if err != nil {
			t.Fatal(err)
		}
		for _, raw := range []json.RawMessage{legacy.NormalizedCommand, legacy.CriticalResourceVersions, legacy.FinancialSnapshot} {
			var object map[string]json.RawMessage
			if err = json.Unmarshal(raw, &object); err != nil || string(object["provider_control_version"]) != "0" {
				t.Fatal("executor projection must retain the approved virtual version", string(raw), err)
			}
		}
	})
	t.Run("proof-era-authorization-wire-preserves-default-version", func(t *testing.T) {
		input := defaultProviderResumeProofInput(t, "PAYMENT_PRODUCT_RESUME", 0)
		artifacts, err := ConvertLegacyProofInput(input)
		if err != nil {
			t.Fatal(err)
		}
		request := uuid.New()
		now := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
		event := AuthorizationEvent{
			EventID: uuid.New(), AggregateID: request, AggregateVersion: 3, OccurredAt: now, TraceID: "default-provider-codec-test",
			ApprovalRequestID: request, CommandType: input.CommandType, CommandFingerprint: artifacts.CommandDigest, PolicyVersion: input.PolicyVersion, ExecutorIdentity: "billing-command-executor", ExecutionDeadline: now.Add(time.Minute),
			NormalizedCommand: extractProofPart(t, artifacts.ProofInputJSON, "normalized_command"), TargetResourceType: input.TargetResourceType, TargetResourceID: input.TargetResourceID,
			CriticalResourceVersions: extractProofPart(t, artifacts.ProofInputJSON, "critical_resource_versions"), FinancialSnapshot: extractProofPart(t, artifacts.ProofInputJSON, "financial_snapshot"),
			ProofSchemaVersion: ProofSchemaVersion, CommandDigest: artifacts.CommandDigest, SnapshotDigest: artifacts.SnapshotDigest, DomainRevision: "controlled-provider-state-0", ProofDigest: strings.Repeat("a", 64), ApprovalControlEpoch: 1,
		}
		binding := ExpectedAuthorizationBinding{ApprovedRequestVersion: 3, ApprovalEpoch: 1}
		wire, err := EncodeAuthorizationEvent(event, binding)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodeAuthorizationEvent(wire, binding)
		if err != nil || decoded.Payload.CommandDigest != artifacts.CommandDigest || !bytes.Equal(decoded.Payload.NormalizedCommand, event.NormalizedCommand) || !bytes.Equal(decoded.Payload.FinancialSnapshot, event.FinancialSnapshot) {
			t.Fatal("real authorization producer/consumer must preserve the exact zero version", err)
		}
	})
	t.Run("persisted-product-provider-version-is-still-valid", func(t *testing.T) {
		if _, err := ConvertLegacyProofInput(defaultProviderResumeProofInput(t, "PAYMENT_PRODUCT_RESUME", 7)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("provider-wide-resume-still-requires-persisted-pause", func(t *testing.T) {
		if _, err := ConvertLegacyProofInput(defaultProviderResumeProofInput(t, "PAYMENT_PROVIDER_RESUME", 0)); err == nil {
			t.Fatal("a provider that was never paused must not obtain a provider-wide resume proof")
		}
	})
	for _, test := range []struct {
		name, field string
		value       int64
	}{
		{"negative-provider-version", "provider_control_version", -1},
		{"zero-product-version", "product_control_version", 0},
		{"zero-catalog-version", "catalog_version", 0},
		{"zero-admission-version", "admission_version", 0},
		{"zero-mapping-version", "mapping_version", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := defaultProviderResumeProofInput(t, "PAYMENT_PRODUCT_RESUME", 0)
			input.NormalizedCommand = defaultProviderProofVersion(t, input.NormalizedCommand, test.field, test.value)
			input.CriticalResourceVersions = defaultProviderProofVersion(t, input.CriticalResourceVersions, test.field, test.value)
			input.FinancialSnapshot = defaultProviderProofVersion(t, input.FinancialSnapshot, test.field, test.value)
			if _, err := ConvertLegacyProofInput(input); err == nil {
				t.Fatal("the default provider exception must not relax another version boundary")
			}
		})
	}
	for _, part := range []string{"critical", "financial"} {
		t.Run("zero-provider-must-match-"+part, func(t *testing.T) {
			input := defaultProviderResumeProofInput(t, "PAYMENT_PRODUCT_RESUME", 0)
			if part == "critical" {
				input.CriticalResourceVersions = defaultProviderProofVersion(t, input.CriticalResourceVersions, "provider_control_version", 1)
			} else {
				input.FinancialSnapshot = defaultProviderProofVersion(t, input.FinancialSnapshot, "provider_control_version", 1)
			}
			if _, err := ConvertLegacyProofInput(input); err == nil {
				t.Fatal("all three approved provider facts must match")
			}
		})
	}
	t.Run("noncanonical-zero-wire-is-still-rejected", func(t *testing.T) {
		input := defaultProviderResumeProofInput(t, "PAYMENT_PRODUCT_RESUME", 0)
		artifacts, err := ConvertLegacyProofInput(input)
		if err != nil {
			t.Fatal(err)
		}
		wire := bytes.ReplaceAll(artifacts.ProofInputJSON, []byte(`"provider_control_version":"0"`), []byte(`"provider_control_version":"00"`))
		if bytes.Equal(wire, artifacts.ProofInputJSON) {
			t.Fatal("negative input did not replace a canonical zero")
		}
		if _, err = DecodeProofInput(input.CommandType, input.PolicyVersion, wire); err == nil {
			t.Fatal("zero remains subject to canonical integer encoding")
		}
	})
}

func defaultProviderResumeProofInput(t *testing.T, command string, version int64) LegacyProofInput {
	t.Helper()
	for _, fixture := range frozenLegacyInputs() {
		if fixture.input.CommandType != command {
			continue
		}
		input := fixture.input
		input.NormalizedCommand = defaultProviderProofVersion(t, input.NormalizedCommand, "provider_control_version", version)
		input.CriticalResourceVersions = defaultProviderProofVersion(t, input.CriticalResourceVersions, "provider_control_version", version)
		input.FinancialSnapshot = defaultProviderProofVersion(t, input.FinancialSnapshot, "provider_control_version", version)
		return input
	}
	t.Fatal("missing existing closed command fixture", command)
	return LegacyProofInput{}
}

func defaultProviderProofVersion(t *testing.T, raw json.RawMessage, field string, value int64) json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	if _, ok := object[field]; ok {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		object[field] = encoded
	}
	result, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
