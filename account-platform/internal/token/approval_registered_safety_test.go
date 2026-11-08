package token

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
)

// Capture every copy actually handed to the consumer, not only the broker's
// original storage: mutating a decoded copy must not evade the frozen-byte check.
type tokenSafetyDelivery struct {
	*tokenAuthorizationDeliveryStub
	issued [][]byte
}

func (delivery *tokenSafetyDelivery) DeliveryBytes() []byte {
	raw := delivery.tokenAuthorizationDeliveryStub.DeliveryBytes()
	delivery.issued = append(delivery.issued, raw)
	return raw
}

func (fixture *tokenAuthorizationHTTPFixture) freezeLegacySafetyWire(t *testing.T, indented bool) {
	t.Helper()
	legacy := fixture.event
	legacy.ProofSchemaVersion, legacy.DomainRevision, legacy.ProofDigest = "", "", ""
	legacy.CommandDigest, legacy.SnapshotDigest, legacy.ApprovalControlEpoch = "", "", 0
	payload, err := json.Marshal(tokenEventFromAuthorization(legacy))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := approvalencoding.EncodeLegacyAuthorizationEvent(events.Event{ID: legacy.EventID, Type: approvalencoding.AuthorizationEventType,
		AggregateID: legacy.ApprovalRequestID, AggregateVersion: fixture.binding.ApprovedRequestVersion, OccurredAt: legacy.OccurredAt, TraceID: legacy.TraceID, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if indented {
		var formatted bytes.Buffer
		if err := json.Indent(&formatted, raw, "", "  "); err != nil || formatted.Len() <= len(raw) {
			t.Fatal("fixture must retain longer formatted frozen bytes")
		}
		raw = formatted.Bytes()
	}
	fixture.raw = append([]byte(nil), raw...)
	sum := sha256.Sum256(raw)
	fixture.binding.EventDigest = hex.EncodeToString(sum[:])
	fixture.lease.AuthorizationEventDigest = fixture.binding.EventDigest
}

func TestTokenAuthorizationRegisteredConsumerSafety(t *testing.T) {
	for _, scenario := range []string{"success_duplicate", "safe", "invalid_lease", "jsonb_whitespace", "large_integer_equal", "large_integer_lease_mismatch"} {
		t.Run(scenario, func(t *testing.T) {
			fixture, server := newTokenAuthorizationHTTPFixture(t, "legacy")
			defer server.Close()
			reader := &approvalActivationStub{state: "ACTIVE", epoch: 1}
			if scenario == "safe" {
				reader.state, reader.epoch = "SAFE_ROLLBACK", 2
			}
			if strings.HasPrefix(scenario, "large_integer") {
				var command credentialRevokeCommand
				var critical credentialRevokeCritical
				if json.Unmarshal(fixture.event.NormalizedCommand, &command) != nil || json.Unmarshal(fixture.event.CriticalResourceVersions, &critical) != nil {
					t.Fatal("invalid original command fixture")
				}
				command.ExpectedAccountVersion, critical.TokenAccountVersion = 9007199254740992, 9007199254740992
				fixture.event.NormalizedCommand, _ = json.Marshal(command)
				fixture.event.CriticalResourceVersions, _ = json.Marshal(critical)
				fixture.lease.NormalizedCommand = append([]byte(nil), fixture.event.NormalizedCommand...)
				if scenario == "large_integer_lease_mismatch" {
					critical.TokenAccountVersion++
				}
				fixture.lease.CriticalResourceVersions, _ = json.Marshal(critical)
			}
			fixture.freezeLegacySafetyWire(t, scenario == "jsonb_whitespace")
			if scenario == "invalid_lease" {
				fixture.lease.ExecutionIdempotencyKey = "private-lease-key-canary"
			}
			consumer, err := NewTokenAuthorizationCommandConsumer(fixture.event.CommandType, fixture.binding.Durable, server.URL, server.URL, "token-workload", reader)
			if err != nil {
				t.Fatal(err)
			}
			frozen := append([]byte(nil), fixture.raw...)
			first := &tokenSafetyDelivery{tokenAuthorizationDeliveryStub: fixture.delivery(1)}
			second := &tokenSafetyDelivery{tokenAuthorizationDeliveryStub: fixture.delivery(2)}
			if first.meta.StreamSequence != second.meta.StreamSequence || first.meta.NumDelivered != 1 || second.meta.NumDelivered != 2 {
				t.Fatal("fixture must model redelivery of the same physical message")
			}
			consumer.Handle(context.Background(), first)
			consumer.Handle(context.Background(), second)
			for _, delivery := range []*tokenSafetyDelivery{first, second} {
				if len(delivery.issued) == 0 {
					t.Fatal("test did not reach the real delivery decoder")
				}
				for _, raw := range delivery.issued {
					if !bytes.Equal(raw, frozen) {
						t.Fatal("consumer mutated the supplied frozen authorization bytes")
					}
				}
				if delivery.publish != 0 {
					t.Fatal("Token consumer published a dead letter")
				}
			}
			rejected := scenario == "safe" || scenario == "invalid_lease" || scenario == "large_integer_lease_mismatch"
			if rejected {
				if first.ack != 0 || second.ack != 0 || first.nak != 1 || second.nak != 1 || fixture.domainCalls != 0 || fixture.completes != 0 || fixture.resultWrites != 0 || fixture.terminals != 0 || fixture.state.State != "READY" {
					t.Fatalf("unconfirmed delivery was executed or acknowledged: ack=%d/%d nak=%d/%d domain=%d complete=%d state=%s", first.ack, second.ack, first.nak, second.nak, fixture.domainCalls, fixture.completes, fixture.state.State)
				}
				if scenario == "safe" && (fixture.begins != 0 || fixture.domainReads != 0) {
					t.Fatal("SAFE reached a lease or Token domain result")
				}
				// Supplement public Handle outcomes with the exact current internal
				// diagnosis; do not expose a lease key or numeric frozen evidence.
				diagnosis := consumer.handle(context.Background(), fixture.delivery(3))
				if !errors.Is(diagnosis, ErrApprovalResultUnconfirmed) || strings.Contains(diagnosis.Error(), "private-lease-key-canary") || strings.Contains(diagnosis.Error(), "900719925474099") {
					t.Fatalf("unconfirmed diagnosis lost its safe contract: %v", diagnosis)
				}
				if scenario != "safe" && diagnosis.Error() != "TOKEN_APPROVAL_RESULT_UNCONFIRMED: APPROVAL_LEASE_BINDING_INVALID" {
					t.Fatalf("mismatched lease escaped the binding gate: %v", diagnosis)
				}
				return
			}
			if first.ack != 1 || second.ack != 1 || first.nak != 0 || second.nak != 0 || fixture.domainCalls != 1 || fixture.begins != 1 || fixture.completes != 1 || fixture.resultWrites != 1 || fixture.terminals != 1 || fixture.state.State != "TERMINAL_CONFIRMED" {
				t.Fatalf("current single-claim/replay behavior changed: ack=%d/%d nak=%d/%d domain=%d begin=%d complete=%d result=%d terminal=%d state=%s", first.ack, second.ack, first.nak, second.nak, fixture.domainCalls, fixture.begins, fixture.completes, fixture.resultWrites, fixture.terminals, fixture.state.State)
			}
		})
	}
}
