package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
)

// The embedded nil interfaces intentionally panic if a non-target delivery
// reaches any binding/state/domain operation instead of the read-only route.
type tokenRouteOnlyAccount struct {
	approvalAuthorizationAccount
	route      ApprovalAuthorizationRoute
	routeReads int
}

func (a *tokenRouteOnlyAccount) ResolveAuthorizationRoute(context.Context, uuid.UUID) (ApprovalAuthorizationRoute, error) {
	a.routeReads++
	return a.route, nil
}

type tokenRouteForbiddenDomain struct{ approvalAuthorizationDomain }
type tokenRouteDelivery struct {
	raw        []byte
	id         string
	metadata   events.AuthorizationDeliveryMetadata
	acks, naks int
	ackError   error
}

func (d *tokenRouteDelivery) DeliveryBytes() []byte                          { return d.raw }
func (d *tokenRouteDelivery) MessageID() string                              { return d.id }
func (d *tokenRouteDelivery) Metadata() events.AuthorizationDeliveryMetadata { return d.metadata }
func (d *tokenRouteDelivery) DoubleAck(context.Context) error                { d.acks++; return d.ackError }
func (d *tokenRouteDelivery) NakWithDelay(time.Duration) error               { d.naks++; return nil }
func (d *tokenRouteDelivery) PublishDeadLetter(context.Context) (events.AuthorizationDLQPubAck, error) {
	panic("non-target delivery published a DLQ")
}

func tokenRouteFixture(t *testing.T, descriptor approvalconsumer.Descriptor, wire string) (ApprovalAuthorizationRoute, []byte) {
	t.Helper()
	now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	id := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	request := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	targetID := "33333333-3333-4333-8333-333333333333"
	targetType := "MODEL"
	fingerprint := strings.Repeat("a", 64)
	financial := map[string]any{"billing_price_revision": 1, "price_set_fingerprint": fingerprint, "models_checked": 1, "models_covered": 1, "fully_covered": true, "billing_as_of": now.Format(time.RFC3339Nano)}
	normalized := map[string]any{}
	critical := map[string]any{}
	switch descriptor.CommandType {
	case "TOKEN_MODEL_LIFECYCLE_CHANGE":
		normalized = map[string]any{"model_id": targetID, "lifecycle_plan_id": id.String(), "lifecycle_plan_version": 1, "target_lifecycle": "DEPRECATED", "effective_at": now.Format(time.RFC3339Nano), "existing_session_cutoff_at": now.Add(time.Hour).Format(time.RFC3339Nano), "replacement_model_id": nil}
		critical = map[string]any{"model_id": targetID, "catalog_version": 1, "current_lifecycle": "PUBLISHED", "target_lifecycle": "DEPRECATED", "lifecycle_plan_version": 1, "effective_at": normalized["effective_at"], "existing_session_cutoff_at": normalized["existing_session_cutoff_at"], "replacement_model_id": nil, "billing_price_version": 1, "route_set_fingerprint": fingerprint, "provisioning_set_fingerprint": fingerprint, "inflight_request_set_fingerprint": fingerprint, "policy_version": 2}
	case "TOKEN_MODEL_CATALOG_ROLLBACK":
		targetID, targetType = "1", "MODEL_CATALOG"
		normalized = map[string]any{"source_catalog_version": 1}
		critical = map[string]any{"current_catalog_version": 2, "source_catalog_version": 1, "catalog_diff_fingerprint": fingerprint, "billing_price_version": 1, "route_set_fingerprint": fingerprint, "provisioning_set_fingerprint": fingerprint, "inflight_request_set_fingerprint": fingerprint, "policy_version": 2}
	case "TOKEN_CREDENTIAL_REVOKE":
		targetID, targetType = id.String(), "PROVIDER_CREDENTIAL"
		normalized = map[string]any{"token_account_id": request.String(), "expected_account_version": 1, "credential_id": targetID, "credential_version": 1}
		critical = map[string]any{"token_account_id": request.String(), "token_account_version": 1, "credential_id": targetID, "credential_version": 1, "credential_status": "ACTIVE", "credential_fingerprint": fingerprint, "active_route_set_fingerprint": fingerprint, "provisioning_lease_set_fingerprint": fingerprint, "inflight_request_set_fingerprint": fingerprint, "replacement_credential_version": nil, "policy_version": 1}
		financial = map[string]any{}
	default:
		t.Fatal("uncovered Token command", descriptor.CommandType)
	}
	marshal := func(value any) json.RawMessage {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	event := approvalencoding.AuthorizationEvent{EventID: id, AggregateID: request, AggregateVersion: 7, OccurredAt: now, TraceID: "billing-shared-subject-routing", ApprovalRequestID: request, CommandType: descriptor.CommandType, CommandFingerprint: fingerprint, PolicyVersion: descriptor.PolicyVersion, ExecutorIdentity: descriptor.ExecutorIdentity, ExecutionDeadline: now.Add(time.Hour), NormalizedCommand: marshal(normalized), TargetResourceType: targetType, TargetResourceID: targetID, CriticalResourceVersions: marshal(critical), FinancialSnapshot: marshal(financial), ProofSchemaVersion: approvalencoding.ProofSchemaVersion, DomainRevision: "routing-fixture:1", ProofDigest: strings.Repeat("b", 64), ApprovalControlEpoch: 1}
	epoch := int64(1)
	serialization := approvalencoding.LegacySerializationVersion
	provenance := "RECONSTRUCTED_FOR_DELIVERY"
	var raw []byte
	var err error
	if wire == "approval" {
		snapshotVersion := 2
		if descriptor.CommandType == "TOKEN_CREDENTIAL_REVOKE" {
			snapshotVersion = 1
		}
		proof, e := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{CommandType: event.CommandType, PolicyVersion: event.PolicyVersion, SnapshotSchemaVersion: snapshotVersion, TargetResourceType: targetType, TargetResourceID: targetID, NormalizedCommand: event.NormalizedCommand, CriticalResourceVersions: event.CriticalResourceVersions, FinancialSnapshot: event.FinancialSnapshot})
		if e != nil {
			t.Fatal(e)
		}
		var projection struct {
			Normalized json.RawMessage `json:"normalized_command"`
			Critical   json.RawMessage `json:"critical_resource_versions"`
			Financial  json.RawMessage `json:"financial_snapshot"`
		}
		if e = json.Unmarshal(proof.ProofInputJSON, &projection); e != nil {
			t.Fatal(e)
		}
		event.NormalizedCommand, event.CriticalResourceVersions, event.FinancialSnapshot = projection.Normalized, projection.Critical, projection.Financial
		event.CommandDigest, event.SnapshotDigest = proof.CommandDigest, proof.SnapshotDigest
		epoch = 3
		event.ApprovalControlEpoch = epoch
		raw, err = approvalencoding.EncodeAuthorizationEvent(event, approvalencoding.ExpectedAuthorizationBinding{ApprovedRequestVersion: 7, ApprovalEpoch: epoch})
		serialization = approvalencoding.ApprovalSerializationVersion
		provenance = "FROZEN_AT_APPROVAL"
	} else {
		payload := marshal(map[string]any{"approval_request_id": request, "command_type": event.CommandType, "command_fingerprint": fingerprint, "policy_version": event.PolicyVersion, "executor_identity": event.ExecutorIdentity, "execution_deadline": event.ExecutionDeadline, "normalized_command": event.NormalizedCommand, "target_resource_type": targetType, "target_resource_id": targetID, "critical_resource_versions": event.CriticalResourceVersions, "financial_snapshot": event.FinancialSnapshot})
		raw, err = approvalencoding.EncodeLegacyAuthorizationEvent(events.Event{ID: id, Type: approvalencoding.AuthorizationEventType, AggregateID: request, AggregateVersion: 7, OccurredAt: now, TraceID: event.TraceID, Payload: payload})
	}
	if err != nil {
		t.Fatal(err)
	}
	// Exercise the production decoder for every fixture, including legacy proof conversion.
	if _, err = approvalencoding.DecodeAuthorizationEvent(raw, approvalencoding.ExpectedAuthorizationBinding{ApprovedRequestVersion: 7, ApprovalEpoch: epoch}); err != nil {
		t.Fatal("fixture is not a valid production authorization", err)
	}
	digest := sha256.Sum256(raw)
	route := ApprovalAuthorizationRoute{EventID: id, ApprovalRequestID: request, ApprovedRequestVersion: 7, ApprovalEpoch: epoch, SerializationVersion: serialization, Provenance: provenance, EventDigest: hex.EncodeToString(digest[:]), DigestKind: "DELIVERY_BYTES", CommandType: event.CommandType, PolicyVersion: event.PolicyVersion, ExecutorIdentity: event.ExecutorIdentity, CanonicalWorkload: descriptor.Workload, CanonicalDurable: descriptor.Durable}
	if wire == "legacy-semantic" {
		decoded, e := approvalencoding.DecodeAuthorizationEvent(raw, approvalencoding.ExpectedAuthorizationBinding{ApprovedRequestVersion: 7, ApprovalEpoch: epoch})
		if e != nil {
			t.Fatal(e)
		}
		semantic, e := (events.Event{ID: decoded.EventID, Type: approvalencoding.AuthorizationEventType, AggregateID: decoded.AggregateID, AggregateVersion: decoded.AggregateVersion, OccurredAt: decoded.OccurredAt, TraceID: decoded.TraceID, Payload: decoded.LegacyPayload}).Marshal()
		if e != nil {
			t.Fatal(e)
		}
		digest = sha256.Sum256(semantic)
		route.EventDigest = hex.EncodeToString(digest[:])
		route.DigestKind = "LEGACY_SEMANTIC"
		route.Provenance = "RECONSTRUCTED_AFTER_PUBLISH"
	}
	return route, raw
}

func tokenRouteConsumer(durable string, route ApprovalAuthorizationRoute, raw []byte) (*AuthorizationCommandConsumer, *tokenRouteOnlyAccount, *tokenRouteDelivery) {
	account := &tokenRouteOnlyAccount{route: route}
	consumer := &AuthorizationCommandConsumer{account: account, domain: &tokenRouteForbiddenDomain{}, durable: durable, workload: approvalconsumer.BillingWorkload}
	delivery := &tokenRouteDelivery{raw: raw, id: route.EventID.String(), metadata: events.AuthorizationDeliveryMetadata{Stream: "ACCOUNT_PLATFORM", StreamSequence: 42, NumDelivered: 1}}
	return consumer, account, delivery
}

func TestBillingAcknowledgesTokenAuthorizationsWithoutExecuting(t *testing.T) {
	if len(approvalconsumer.BillingCommands()) != 11 || len(approvalconsumer.TokenCommands()) != 3 {
		t.Fatal("registry changed: extend the cross-workload routing scenarios")
	}
	for _, token := range approvalconsumer.TokenCommands() {
		for _, wire := range []string{"legacy", "approval"} {
			route, raw := tokenRouteFixture(t, token, wire)
			for _, billing := range approvalconsumer.BillingCommands() {
				t.Run(token.CommandType+"/"+wire+"/"+billing.Durable, func(t *testing.T) {
					consumer, account, delivery := tokenRouteConsumer(billing.Durable, route, raw)
					consumer.Handle(context.Background(), delivery)
					if delivery.acks != 1 || delivery.naks != 0 || account.routeReads != 1 {
						t.Fatalf("non-target route: ack=%d nak=%d routeReads=%d", delivery.acks, delivery.naks, account.routeReads)
					}
				})
			}
		}
	}
}

func TestBillingTokenRoutingRejectsUnverifiedDeliveries(t *testing.T) {
	token := approvalconsumer.TokenCommands()[0]
	base, raw := tokenRouteFixture(t, token, "approval")
	cases := map[string]func(*ApprovalAuthorizationRoute, *tokenRouteDelivery){
		"wrong-durable": func(r *ApprovalAuthorizationRoute, _ *tokenRouteDelivery) {
			r.CanonicalDurable = approvalconsumer.BillingCommands()[0].Durable
		},
		"wrong-workload": func(r *ApprovalAuthorizationRoute, _ *tokenRouteDelivery) {
			r.CanonicalWorkload = approvalconsumer.BillingWorkload
		},
		"wrong-executor": func(r *ApprovalAuthorizationRoute, _ *tokenRouteDelivery) {
			r.ExecutorIdentity = approvalconsumer.BillingWorkload
		},
		"wrong-policy":    func(r *ApprovalAuthorizationRoute, _ *tokenRouteDelivery) { r.PolicyVersion++ },
		"wrong-event":     func(r *ApprovalAuthorizationRoute, _ *tokenRouteDelivery) { r.EventID = uuid.New() },
		"wrong-aggregate": func(r *ApprovalAuthorizationRoute, _ *tokenRouteDelivery) { r.ApprovalRequestID = uuid.New() },
		"wrong-version":   func(r *ApprovalAuthorizationRoute, _ *tokenRouteDelivery) { r.ApprovedRequestVersion++ },
		"wrong-epoch":     func(r *ApprovalAuthorizationRoute, _ *tokenRouteDelivery) { r.ApprovalEpoch++ },
		"wrong-digest":    func(r *ApprovalAuthorizationRoute, _ *tokenRouteDelivery) { r.EventDigest = strings.Repeat("0", 64) },
		"changed-bytes": func(_ *ApprovalAuthorizationRoute, d *tokenRouteDelivery) {
			d.raw = append(append([]byte(nil), d.raw...), ' ')
		},
		"wrong-stream":           func(_ *ApprovalAuthorizationRoute, d *tokenRouteDelivery) { d.metadata.Stream = "OTHER" },
		"missing-sequence":       func(_ *ApprovalAuthorizationRoute, d *tokenRouteDelivery) { d.metadata.StreamSequence = 0 },
		"missing-delivery-count": func(_ *ApprovalAuthorizationRoute, d *tokenRouteDelivery) { d.metadata.NumDelivered = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			consumer, account, delivery := tokenRouteConsumer(approvalconsumer.BillingCommands()[0].Durable, base, raw)
			mutate(&account.route, delivery)
			consumer.Handle(context.Background(), delivery)
			if delivery.acks != 0 || delivery.naks != 1 {
				t.Fatalf("unverified route acknowledged: ack=%d nak=%d", delivery.acks, delivery.naks)
			}
		})
	}
}

func TestBillingTokenRoutingKeepsLegacySemanticDigestAndAckRetry(t *testing.T) {
	route, raw := tokenRouteFixture(t, approvalconsumer.TokenCommands()[2], "legacy-semantic")
	consumer, account, delivery := tokenRouteConsumer(approvalconsumer.BillingCommands()[0].Durable, route, raw)
	delivery.ackError = errors.New("synthetic lost ACK reply")
	consumer.Handle(context.Background(), delivery)
	if delivery.acks != 1 || delivery.naks != 1 {
		t.Fatal("failed ACK did not leave the message retryable")
	}
	delivery.ackError = nil
	consumer.Handle(context.Background(), delivery)
	if delivery.acks != 2 || delivery.naks != 1 || account.routeReads != 2 {
		t.Fatal("redelivery did not reverify and acknowledge the same route")
	}
}
