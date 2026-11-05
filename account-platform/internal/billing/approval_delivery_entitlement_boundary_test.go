package billing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
)

// Exercise the actual wire decoder and the consumer's binding/target boundary,
// rather than calling the domain execution adapter directly.
func TestEntitlementAuthorizationConsumerDecodedBoundary(t *testing.T) {
	binding, event, _, _ := entitlementCorrectionCommandFixture()
	artifacts, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{
		CommandType: event.CommandType, PolicyVersion: 2, SnapshotSchemaVersion: 2,
		TargetResourceType: event.TargetResourceType, TargetResourceID: event.TargetResourceID,
		NormalizedCommand: event.NormalizedCommand, CriticalResourceVersions: event.CriticalResourceVersions,
		FinancialSnapshot: event.FinancialSnapshot,
	})
	if err != nil {
		t.Fatal(err)
	}
	var proof struct {
		Normalized json.RawMessage `json:"normalized_command"`
		Critical   json.RawMessage `json:"critical_resource_versions"`
		Financial  json.RawMessage `json:"financial_snapshot"`
	}
	if err := json.Unmarshal(artifacts.ProofInputJSON, &proof); err != nil {
		t.Fatal(err)
	}
	event.NormalizedCommand, event.CriticalResourceVersions, event.FinancialSnapshot = proof.Normalized, proof.Critical, proof.Financial
	event.AggregateID, event.AggregateVersion = binding.ApprovalRequestID, 3
	event.OccurredAt, event.ExecutionDeadline = time.Now().UTC(), time.Now().UTC().Add(time.Minute)
	event.TraceID, event.CommandFingerprint, event.ProofDigest = "entitlement-consumer-boundary", strings.Repeat("a", 64), strings.Repeat("f", 64)
	event.ProofSchemaVersion, event.DomainRevision = approvalencoding.ProofSchemaVersion, "1:"+strings.Repeat("c", 64)
	event.CommandDigest, event.SnapshotDigest = artifacts.CommandDigest, artifacts.SnapshotDigest
	expected := approvalencoding.ExpectedAuthorizationBinding{ApprovedRequestVersion: 3, ApprovalEpoch: binding.ApprovalEpoch}
	raw, err := approvalencoding.EncodeAuthorizationEvent(event, expected)
	if err != nil {
		t.Fatal("encode actual entitlement authorization", err)
	}
	decoded, err := approvalencoding.DecodeAuthorizationEvent(raw, expected)
	if err != nil {
		t.Fatal("decode actual entitlement authorization", err)
	}
	descriptor, ok := approvalconsumer.LookupDurable("billing-entitlement-component-adjustment-executor-p2-v1")
	if !ok {
		t.Fatal("entitlement consumer is not installed")
	}
	binding.Durable, binding.Workload = descriptor.Durable, descriptor.Workload
	binding.SerializationVersion, binding.ApprovedRequestVersion = approvalencoding.ApprovalSerializationVersion, 3
	binding.DigestKind = "DELIVERY_BYTES"
	digest := sha256.Sum256(raw)
	binding.EventDigest = hex.EncodeToString(digest[:])
	consumer := &AuthorizationCommandConsumer{durable: descriptor.Durable, workload: descriptor.Workload}
	t.Run("installed-v2-entitlement-target", func(t *testing.T) {
		if !consumer.validBinding(binding) || !consumer.validDecoded(binding, decoded, raw) {
			t.Fatal("real decoded entitlement delivery cannot reach the domain")
		}
	})
	for _, tc := range []struct {
		name string
		id   string
		kind string
	}{
		{"wrong-resource", event.TargetResourceID, "LEDGER_POSTING"},
		{"nil-id", uuid.Nil.String(), "ENTITLEMENT_PACKAGE"},
		{"noncanonical-id", "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA", "ENTITLEMENT_PACKAGE"},
		{"spaced-id", " " + event.TargetResourceID, "ENTITLEMENT_PACKAGE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := decoded
			bad.Payload.TargetResourceID, bad.Payload.TargetResourceType = tc.id, tc.kind
			if consumer.validDecoded(binding, bad, raw) {
				t.Fatal("invalid entitlement target crossed the consumer boundary")
			}
		})
	}
	t.Run("immutable-v1-history-not-executable", func(t *testing.T) {
		old := binding
		old.PolicyVersion = 1
		if consumer.validBinding(old) {
			t.Fatal("historical inactive v1 accepted by the v2 consumer")
		}
	})
	t.Run("another-approval-rejected", func(t *testing.T) {
		other := binding
		other.ApprovalRequestID = uuid.New()
		if consumer.validDecoded(other, decoded, raw) {
			t.Fatal("authorization borrowed another request binding")
		}
	})
	t.Run("changed-delivery-bytes-rejected", func(t *testing.T) {
		if consumer.validDecoded(binding, decoded, append(append([]byte(nil), raw...), ' ')) {
			t.Fatal("delivery digest no longer pins the immutable authorization")
		}
	})
}
