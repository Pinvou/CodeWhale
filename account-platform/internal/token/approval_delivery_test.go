package token

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
)

type tokenAuthorizationDeliveryStub struct {
	raw               []byte
	id                string
	meta              events.AuthorizationDeliveryMetadata
	ack, nak, publish int
	ackErr, nakErr    error
}

func (delivery *tokenAuthorizationDeliveryStub) DeliveryBytes() []byte {
	return append([]byte(nil), delivery.raw...)
}
func (delivery *tokenAuthorizationDeliveryStub) MessageID() string { return delivery.id }
func (delivery *tokenAuthorizationDeliveryStub) Metadata() events.AuthorizationDeliveryMetadata {
	return delivery.meta
}
func (delivery *tokenAuthorizationDeliveryStub) DoubleAck(context.Context) error {
	delivery.ack++
	return delivery.ackErr
}
func (delivery *tokenAuthorizationDeliveryStub) NakWithDelay(time.Duration) error {
	delivery.nak++
	return delivery.nakErr
}
func (delivery *tokenAuthorizationDeliveryStub) PublishDeadLetter(context.Context) (events.AuthorizationDLQPubAck, error) {
	delivery.publish++
	return events.AuthorizationDLQPubAck{}, errors.New("Token delivery has no DLQ capability")
}

type tokenAuthorizationHTTPFixture struct {
	testing *testing.T
	mu      sync.Mutex

	binding tokenAuthorizationBinding
	event   approvalencoding.AuthorizationEvent
	raw     []byte
	lease   tokenApprovalLease
	domain  TokenCommandResult
	state   tokenAuthorizationDeliveryState

	beginPersisted, domainPersisted, completePersisted      bool
	routeReads, bindingReads, observes                      int
	domainCalls, begins, completes, resultWrites, terminals int
	failStage                                               string
}

func newTokenAuthorizationHTTPFixture(t *testing.T, wire string) (*tokenAuthorizationHTTPFixture, *httptest.Server) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	eventID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	requestID := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	accountID := uuid.MustParse("33333333-3333-4333-8333-333333333333")
	credentialID := uuid.MustParse("44444444-4444-4444-8444-444444444444")
	requester := uuid.MustParse("55555555-5555-4555-8555-555555555555")
	approver := uuid.MustParse("66666666-6666-4666-8666-666666666666")
	fingerprint := strings.Repeat("a", 64)
	command, _ := json.Marshal(credentialRevokeCommand{TokenAccountID: accountID, ExpectedAccountVersion: 3, CredentialID: credentialID, CredentialVersion: 1})
	critical, _ := json.Marshal(credentialRevokeCritical{TokenAccountID: accountID, TokenAccountVersion: 3, CredentialID: credentialID, CredentialVersion: 1, CredentialStatus: "ACTIVE", CredentialFingerprint: strings.Repeat("b", 64), ActiveRouteSetFingerprint: fingerprint, ProvisioningLeaseSetFingerprint: fingerprint, InflightRequestSetFingerprint: fingerprint, PolicyVersion: 1})
	legacyCommand, legacyCritical := append(json.RawMessage(nil), command...), append(json.RawMessage(nil), critical...)
	event := approvalencoding.AuthorizationEvent{EventID: eventID, AggregateID: requestID, AggregateVersion: 7, OccurredAt: now,
		TraceID: "token-authorization-test", ApprovalRequestID: requestID, CommandType: "TOKEN_CREDENTIAL_REVOKE",
		CommandFingerprint: fingerprint, PolicyVersion: 1, ExecutorIdentity: approvalconsumer.TokenWorkload,
		ExecutionDeadline: now.Add(time.Hour), NormalizedCommand: command, TargetResourceType: "PROVIDER_CREDENTIAL",
		TargetResourceID: credentialID.String(), CriticalResourceVersions: critical, FinancialSnapshot: json.RawMessage(`{}`),
		ProofSchemaVersion: approvalencoding.ProofSchemaVersion, DomainRevision: "token-credential-revision-1",
		ProofDigest: strings.Repeat("c", 64), ApprovalControlEpoch: 1}
	expected := approvalencoding.ExpectedAuthorizationBinding{ApprovedRequestVersion: 7, ApprovalEpoch: 1}
	var raw []byte
	serialization, provenance, digestKind := approvalencoding.LegacySerializationVersion, "FROZEN_AT_APPROVAL", "DELIVERY_BYTES"
	if wire == "approval" {
		proof, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{CommandType: event.CommandType, PolicyVersion: event.PolicyVersion, SnapshotSchemaVersion: 1,
			TargetResourceType: event.TargetResourceType, TargetResourceID: event.TargetResourceID, NormalizedCommand: command,
			CriticalResourceVersions: critical, FinancialSnapshot: json.RawMessage(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		var projection struct {
			Normalized json.RawMessage `json:"normalized_command"`
			Critical   json.RawMessage `json:"critical_resource_versions"`
			Financial  json.RawMessage `json:"financial_snapshot"`
		}
		if err := json.Unmarshal(proof.ProofInputJSON, &projection); err != nil {
			t.Fatal(err)
		}
		event.NormalizedCommand, event.CriticalResourceVersions, event.FinancialSnapshot = projection.Normalized, projection.Critical, projection.Financial
		event.CommandDigest, event.SnapshotDigest = proof.CommandDigest, proof.SnapshotDigest
		raw, err = approvalencoding.EncodeAuthorizationEvent(event, expected)
		if err != nil {
			t.Fatal(err)
		}
		serialization = approvalencoding.ApprovalSerializationVersion
	} else {
		legacyEvent := event
		legacyEvent.ProofSchemaVersion, legacyEvent.DomainRevision, legacyEvent.ProofDigest = "", "", ""
		legacyEvent.ApprovalControlEpoch = 0
		payload, _ := json.Marshal(tokenEventFromAuthorization(legacyEvent))
		var err error
		raw, err = approvalencoding.EncodeLegacyAuthorizationEvent(events.Event{ID: eventID, Type: approvalencoding.AuthorizationEventType, AggregateID: requestID, AggregateVersion: 7, OccurredAt: now, TraceID: event.TraceID, Payload: payload})
		if err != nil {
			t.Fatal(err)
		}
	}
	event.NormalizedCommand, event.CriticalResourceVersions, event.FinancialSnapshot = legacyCommand, legacyCritical, json.RawMessage(`{}`)
	command, critical = legacyCommand, legacyCritical
	sum := sha256.Sum256(raw)
	binding := tokenAuthorizationBinding{EventID: eventID, Durable: "token-credential-revoke-executor-v1", Workload: approvalconsumer.TokenWorkload,
		ApprovalRequestID: requestID, ApprovedRequestVersion: 7, ApprovalEpoch: 1, SerializationVersion: serialization,
		Provenance: provenance, EventDigest: hex.EncodeToString(sum[:]), DigestKind: digestKind, CommandType: event.CommandType,
		PolicyVersion: event.PolicyVersion, ExecutorIdentity: approvalconsumer.TokenWorkload}
	lease := tokenApprovalLease{ApprovalRequestID: requestID, CommandType: event.CommandType, PolicyVersion: event.PolicyVersion,
		CommandFingerprint: fingerprint, NormalizedCommand: command, TargetResourceType: event.TargetResourceType, TargetResourceID: event.TargetResourceID,
		CriticalResourceVersions: critical, FinancialSnapshot: json.RawMessage(`{}`), RequesterUserID: requester, RequesterAuthorizationVersion: 9,
		RequesterReason: "A sufficiently detailed credential revocation reason", ApproverUserID: approver, DecisionReason: "Independent review passed",
		ExecutionIdempotencyKey: "approval:" + requestID.String() + ":" + event.CommandType + ":" + fingerprint,
		ExecutorIdentity:        approvalconsumer.TokenWorkload, Status: "EXECUTING", AuthorizationEventID: &eventID,
		AuthorizationDurable: binding.Durable, AuthorizationApprovedVersion: 7, AuthorizationEventDigest: binding.EventDigest,
		AuthorizationDigestKind: binding.DigestKind, AuthorizationSerialization: binding.SerializationVersion,
		AuthorizationProvenance: binding.Provenance, AuthorizationApprovalEpoch: 1}
	domain := TokenCommandResult{CommandReference: uuid.MustParse("77777777-7777-4777-8777-777777777777"), CommandType: event.CommandType,
		PolicyVersion: event.PolicyVersion, ActorUserID: requester, AuthorizationVersion: 9, TargetResourceType: event.TargetResourceType,
		TargetResourceID: event.TargetResourceID, Status: "SUCCEEDED", ApprovalRequestID: &requestID, CreatedAt: now, CompletedAt: &now}
	fixture := &tokenAuthorizationHTTPFixture{testing: t, binding: binding, event: event, raw: raw, lease: lease, domain: domain,
		state: tokenAuthorizationDeliveryState{State: "READY", Version: 1}}
	server := httptest.NewServer(http.HandlerFunc(fixture.handleHTTP))
	return fixture, server
}

func (fixture *tokenAuthorizationHTTPFixture) handleHTTP(w http.ResponseWriter, request *http.Request) {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	if request.Header.Get("Authorization") == "" {
		fixture.testing.Error("missing service authentication")
	}
	path := request.URL.Path
	switch {
	case strings.HasSuffix(path, "/route"):
		fixture.routeReads++
		_ = json.NewEncoder(w).Encode(tokenAuthorizationRoute{EventID: fixture.binding.EventID,
			ApprovalRequestID: fixture.binding.ApprovalRequestID, ApprovedRequestVersion: fixture.binding.ApprovedRequestVersion,
			ApprovalEpoch: fixture.binding.ApprovalEpoch, SerializationVersion: fixture.binding.SerializationVersion,
			Provenance: fixture.binding.Provenance, EventDigest: fixture.binding.EventDigest, DigestKind: fixture.binding.DigestKind,
			CommandType: fixture.binding.CommandType, PolicyVersion: fixture.binding.PolicyVersion,
			ExecutorIdentity: fixture.binding.ExecutorIdentity, CanonicalWorkload: fixture.binding.Workload,
			CanonicalDurable: fixture.binding.Durable})
	case strings.HasSuffix(path, "/binding"):
		fixture.bindingReads++
		_ = json.NewEncoder(w).Encode(fixture.binding)
	case strings.HasSuffix(path, "/observe"):
		fixture.observes++
		_ = json.NewEncoder(w).Encode(fixture.state)
	case strings.HasSuffix(path, "/result") && strings.Contains(path, "/commands/"):
		if !fixture.domainPersisted {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(fixture.domain)
	case strings.HasSuffix(path, "/precheck"):
		_ = json.NewEncoder(w).Encode(ApprovalSnapshotProjection{CommandType: fixture.event.CommandType, TargetResourceType: fixture.event.TargetResourceType,
			TargetResourceID: fixture.event.TargetResourceID, CriticalResourceVersions: fixture.event.CriticalResourceVersions,
			FinancialSnapshot: fixture.event.FinancialSnapshot, PrecheckFingerprint: fixture.event.CommandFingerprint, TokenObservedAt: time.Now().UTC()})
	case strings.HasSuffix(path, "/executions"):
		fixture.begins++
		first := !fixture.beginPersisted
		fixture.beginPersisted = true
		fixture.lease.FirstClaim = first
		if fixture.completePersisted {
			fixture.lease.Status = "EXECUTED"
		}
		if fixture.failStage == "begin-response-lost" && fixture.begins == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(fixture.lease)
	case strings.HasSuffix(path, "/execute"):
		var body approvedTokenCommandBody
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			fixture.testing.Fatal(err)
		}
		if fixture.binding.SerializationVersion == approvalencoding.ApprovalSerializationVersion {
			if body.DomainRevision != fixture.event.DomainRevision || body.ProofDigest != fixture.event.ProofDigest ||
				body.CommandDigest != fixture.event.CommandDigest || body.SnapshotDigest != fixture.event.SnapshotDigest ||
				body.ApprovalControlEpoch != fixture.event.ApprovalControlEpoch {
				fixture.testing.Fatalf("proof binding lost before Token execution: %+v", body)
			}
		} else if body.DomainRevision != "" || body.ProofDigest != "" || body.ApprovalControlEpoch != 0 {
			fixture.testing.Fatalf("legacy authorization acquired proof fields: %+v", body)
		}
		fixture.domainCalls++
		if fixture.failStage != "execute-unknown" {
			fixture.domainPersisted = true
		}
		if fixture.failStage == "execute-unknown" || fixture.failStage == "execute-response-lost" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(fixture.domain)
	case strings.HasSuffix(path, "/executions/complete"):
		fixture.completes++
		fixture.completePersisted = true
		if fixture.failStage == "complete-response-lost" && fixture.completes == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case strings.HasSuffix(path, "/deliveries/"+fixture.binding.Durable+"/result"):
		fixture.resultWrites++
		fixture.state = tokenAuthorizationDeliveryState{State: "RESULT_CONFIRMED", Version: fixture.state.Version + 1}
		if fixture.failStage == "result-response-lost" && fixture.resultWrites == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(fixture.state)
	case strings.HasSuffix(path, "/terminal"):
		fixture.terminals++
		fixture.state = tokenAuthorizationDeliveryState{State: "TERMINAL_CONFIRMED", Version: fixture.state.Version + 1}
		if fixture.failStage == "terminal-response-lost" && fixture.terminals == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(fixture.state)
	default:
		fixture.testing.Errorf("unexpected %s %s", request.Method, request.URL.String())
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestTokenAuthorizationConsumerAcknowledgesAuthenticatedNonTargetWithoutStateOrDomain(t *testing.T) {
	fixture, server := newTokenAuthorizationHTTPFixture(t, "approval")
	defer server.Close()
	consumer, err := NewTokenAuthorizationCommandConsumer("TOKEN_MODEL_LIFECYCLE_CHANGE", "token-model-lifecycle-executor-v1",
		server.URL, server.URL, "token-workload", &approvalActivationStub{state: "ACTIVE", epoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	delivery := fixture.delivery(1)
	consumer.Handle(context.Background(), delivery)
	if delivery.ack != 1 || delivery.nak != 0 || delivery.publish != 0 || fixture.routeReads != 1 ||
		fixture.bindingReads != 0 || fixture.observes != 0 || fixture.begins != 0 || fixture.domainCalls != 0 {
		t.Fatalf("non-target outcome ack=%d nak=%d publish=%d route=%d binding=%d observe=%d begin=%d domain=%d",
			delivery.ack, delivery.nak, delivery.publish, fixture.routeReads, fixture.bindingReads, fixture.observes, fixture.begins, fixture.domainCalls)
	}
}

func (fixture *tokenAuthorizationHTTPFixture) delivery(number uint64) *tokenAuthorizationDeliveryStub {
	return &tokenAuthorizationDeliveryStub{raw: fixture.raw, id: fixture.binding.EventID.String(),
		meta: events.AuthorizationDeliveryMetadata{Stream: "ACCOUNT_PLATFORM", StreamSequence: 40 + number, NumDelivered: number}}
}

func TestTokenAuthorizationRawConsumerDualWire(t *testing.T) {
	for _, wire := range []string{"legacy", "approval"} {
		t.Run(wire, func(t *testing.T) {
			fixture, server := newTokenAuthorizationHTTPFixture(t, wire)
			defer server.Close()
			consumer, err := NewTokenAuthorizationCommandConsumer(fixture.event.CommandType, fixture.binding.Durable, server.URL, server.URL, "token-workload", &approvalActivationStub{state: "ACTIVE", epoch: 1})
			if err != nil {
				t.Fatal(err)
			}
			delivery := fixture.delivery(1)
			if err := consumer.handle(context.Background(), delivery); err != nil {
				t.Fatal(err)
			}
			if delivery.ack != 1 || delivery.nak != 0 || delivery.publish != 0 || fixture.domainCalls != 1 || fixture.state.State != "TERMINAL_CONFIRMED" {
				t.Fatalf("raw wire did not close safely ack=%d nak=%d publish=%d calls=%d state=%s", delivery.ack, delivery.nak, delivery.publish, fixture.domainCalls, fixture.state.State)
			}
		})
	}
}

func TestTokenAuthorizationRawConsumerCrashRecoveryAndUnlimitedNAK(t *testing.T) {
	for _, stage := range []string{"begin-response-lost", "execute-unknown", "execute-response-lost", "complete-response-lost", "result-response-lost", "ack-unconfirmed", "terminal-response-lost"} {
		t.Run(stage, func(t *testing.T) {
			fixture, server := newTokenAuthorizationHTTPFixture(t, "legacy")
			defer server.Close()
			fixture.failStage = stage
			consumer, err := NewTokenAuthorizationCommandConsumer(fixture.event.CommandType, fixture.binding.Durable, server.URL, server.URL, "token-workload", &approvalActivationStub{state: "ACTIVE", epoch: 1})
			if err != nil {
				t.Fatal(err)
			}
			first := fixture.delivery(1)
			if stage == "ack-unconfirmed" {
				first.ackErr = errors.New("ack outcome unknown")
			}
			consumer.Handle(context.Background(), first)
			callsAfterFirst := fixture.domainCalls
			second := fixture.delivery(2)
			consumer.Handle(context.Background(), second)
			if fixture.domainCalls > 1 || second.publish != 0 {
				t.Fatalf("stage %s repeated domain or published DLQ calls=%d publish=%d", stage, fixture.domainCalls, second.publish)
			}
			switch stage {
			case "begin-response-lost", "execute-unknown":
				if second.ack != 0 || second.nak != 1 || fixture.state.State != "READY" {
					t.Fatalf("stage %s must remain unlimited NAK: ack=%d nak=%d state=%s", stage, second.ack, second.nak, fixture.state.State)
				}
			case "terminal-response-lost":
				if first.nak != 0 || second.ack != 1 || fixture.state.State != "TERMINAL_CONFIRMED" {
					t.Fatalf("terminal response loss did not preserve acknowledged boundary")
				}
			default:
				if second.ack != 1 || second.nak != 0 || fixture.state.State != "TERMINAL_CONFIRMED" {
					t.Fatalf("stage %s did not converge ack=%d nak=%d state=%s", stage, second.ack, second.nak, fixture.state.State)
				}
			}
			if callsAfterFirst > 1 {
				t.Fatalf("stage %s called domain more than once on first delivery", stage)
			}
		})
	}
}

func TestTokenAuthorizationRawConsumerRejectsBindingBeforeDomain(t *testing.T) {
	fixture, server := newTokenAuthorizationHTTPFixture(t, "legacy")
	defer server.Close()
	fixture.binding.EventDigest = strings.Repeat("0", 64)
	consumer, err := NewTokenAuthorizationCommandConsumer(fixture.event.CommandType, fixture.binding.Durable, server.URL, server.URL, "token-workload", &approvalActivationStub{state: "ACTIVE", epoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	delivery := fixture.delivery(1)
	consumer.Handle(context.Background(), delivery)
	if delivery.ack != 0 || delivery.nak != 1 || delivery.publish != 0 || fixture.domainCalls != 0 || fixture.begins != 0 {
		t.Fatalf("invalid binding reached domain ack=%d nak=%d publish=%d calls=%d begins=%d", delivery.ack, delivery.nak, delivery.publish, fixture.domainCalls, fixture.begins)
	}
}
