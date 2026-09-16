package token

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/tokengovernance"
	"github.com/google/uuid"
)

const tokenAuthorizationRetryMaximumExponent = 9

var errTokenAuthorizationAlreadyAcknowledged = errors.New("token authorization delivery was acknowledged before terminal state persistence")

type tokenAuthorizationBinding struct {
	EventID                uuid.UUID `json:"event_id"`
	Durable                string    `json:"durable"`
	Workload               string    `json:"workload"`
	ApprovalRequestID      uuid.UUID `json:"approval_request_id"`
	ApprovedRequestVersion int64     `json:"approved_request_version"`
	ApprovalEpoch          int64     `json:"approval_epoch"`
	SerializationVersion   string    `json:"serialization_version"`
	Provenance             string    `json:"provenance"`
	EventDigest            string    `json:"event_digest"`
	DigestKind             string    `json:"digest_kind"`
	CommandType            string    `json:"command_type"`
	PolicyVersion          int64     `json:"policy_version"`
	ExecutorIdentity       string    `json:"executor_identity"`
}

type tokenAuthorizationRoute struct {
	EventID                uuid.UUID `json:"event_id"`
	ApprovalRequestID      uuid.UUID `json:"approval_request_id"`
	ApprovedRequestVersion int64     `json:"approved_request_version"`
	ApprovalEpoch          int64     `json:"approval_epoch"`
	SerializationVersion   string    `json:"serialization_version"`
	Provenance             string    `json:"provenance"`
	EventDigest            string    `json:"event_digest"`
	DigestKind             string    `json:"digest_kind"`
	CommandType            string    `json:"command_type"`
	PolicyVersion          int64     `json:"policy_version"`
	ExecutorIdentity       string    `json:"executor_identity"`
	CanonicalWorkload      string    `json:"canonical_workload"`
	CanonicalDurable       string    `json:"canonical_durable"`
}

func (route tokenAuthorizationRoute) binding() tokenAuthorizationBinding {
	return tokenAuthorizationBinding{EventID: route.EventID, Durable: route.CanonicalDurable, Workload: route.CanonicalWorkload,
		ApprovalRequestID: route.ApprovalRequestID, ApprovedRequestVersion: route.ApprovedRequestVersion, ApprovalEpoch: route.ApprovalEpoch,
		SerializationVersion: route.SerializationVersion, Provenance: route.Provenance, EventDigest: route.EventDigest,
		DigestKind: route.DigestKind, CommandType: route.CommandType, PolicyVersion: route.PolicyVersion, ExecutorIdentity: route.ExecutorIdentity}
}

type tokenAuthorizationDeliveryState struct {
	DomainAttemptCount  int16   `json:"domain_attempt_count"`
	State               string  `json:"state"`
	Version             int64   `json:"version"`
	LastErrorCode       *string `json:"last_error_code,omitempty"`
	ExecutionStatus     *string `json:"execution_status,omitempty"`
	ExecutionErrorClass *string `json:"execution_error_class,omitempty"`
}

// TokenAuthorizationCommandConsumer is the raw authorization-message adapter.
// Token keeps its existing result-first, single-claim execution contract: it
// never spends a Billing attempt and never publishes a Billing dead letter.
type TokenAuthorizationCommandConsumer struct {
	executor *TokenApprovalConsumer
	durable  string
}

func NewTokenAuthorizationCommandConsumer(commandType, durable, tokenURL, accountURL, serviceToken string, activation tokengovernance.Reader) (*TokenAuthorizationCommandConsumer, error) {
	descriptor, ok := approvalconsumer.LookupDurable(durable)
	if !ok || descriptor.Workload != approvalconsumer.TokenWorkload || descriptor.CommandType != commandType {
		return nil, errors.New("invalid Token authorization consumer configuration")
	}
	executor, err := NewTokenApprovalConsumer(commandType, tokenURL, accountURL, serviceToken, activation)
	if err != nil {
		return nil, err
	}
	return &TokenAuthorizationCommandConsumer{executor: executor, durable: durable}, nil
}

func (consumer *TokenAuthorizationCommandConsumer) Handle(ctx context.Context, delivery events.AuthorizationRawDelivery) {
	if consumer == nil || consumer.executor == nil || delivery == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := consumer.handle(ctx, delivery); err != nil {
		slog.Error("TOKEN_APPROVAL_AUTHORIZATION_DELIVERY_PENDING")
		if !errors.Is(err, errTokenAuthorizationAlreadyAcknowledged) {
			consumer.nak(delivery)
		}
	}
}

func (consumer *TokenAuthorizationCommandConsumer) handle(ctx context.Context, delivery events.AuthorizationRawDelivery) error {
	eventID, err := uuid.Parse(delivery.MessageID())
	if err != nil || eventID == uuid.Nil || eventID.String() != delivery.MessageID() {
		return errors.New("invalid authorization message identity")
	}
	route, err := consumer.resolveRoute(ctx, eventID)
	if err != nil || !validTokenAuthorizationRoute(route) {
		return errors.New("authorization route unavailable")
	}
	routeBinding := route.binding()
	decoded, err := approvalencoding.DecodeAuthorizationEvent(delivery.DeliveryBytes(), approvalencoding.ExpectedAuthorizationBinding{
		ApprovedRequestVersion: route.ApprovedRequestVersion, ApprovalEpoch: route.ApprovalEpoch,
	})
	if err != nil || !consumer.validDecoded(routeBinding, decoded, delivery.DeliveryBytes()) {
		return errors.New("authorization wire does not match binding")
	}
	metadata := delivery.Metadata()
	sequence := approvalconsumer.Sequence(metadata.StreamSequence)
	if metadata.Stream != "ACCOUNT_PLATFORM" || sequence == 0 || metadata.NumDelivered == 0 {
		return errors.New("invalid authorization delivery metadata")
	}
	if route.CanonicalWorkload != approvalconsumer.TokenWorkload || route.CanonicalDurable != consumer.durable {
		return delivery.DoubleAck(ctx)
	}
	binding, err := consumer.resolveBinding(ctx, eventID)
	if err != nil || !consumer.validBinding(binding) || binding != routeBinding {
		return errors.New("authorization binding unavailable")
	}
	executionEvent := decoded.Payload
	if decoded.SerializationVersion == approvalencoding.ApprovalSerializationVersion {
		legacy, conversionErr := approvalencoding.LegacyExecutionInput(decoded)
		if conversionErr != nil {
			return errors.New("authorization execution projection unavailable")
		}
		executionEvent.NormalizedCommand = legacy.NormalizedCommand
		executionEvent.CriticalResourceVersions = legacy.CriticalResourceVersions
		executionEvent.FinancialSnapshot = legacy.FinancialSnapshot
	}
	state, err := consumer.observe(ctx, binding, metadata.Stream, sequence)
	if err != nil || !validTokenAuthorizationState(state) {
		return errors.New("authorization delivery observation unavailable")
	}
	return consumer.advance(ctx, delivery, binding, executionEvent, state)
}

func validTokenAuthorizationRoute(route tokenAuthorizationRoute) bool {
	binding := route.binding()
	descriptor, ok := approvalconsumer.LookupDurable(route.CanonicalDurable)
	if !ok || binding.EventID == uuid.Nil || binding.ApprovalRequestID == uuid.Nil || binding.ApprovedRequestVersion <= 0 || binding.ApprovalEpoch <= 0 ||
		binding.Workload != binding.ExecutorIdentity || !approvalconsumer.MatchRoute(descriptor, binding.CommandType, binding.PolicyVersion, binding.ExecutorIdentity) {
		return false
	}
	if descriptor.StartSource == approvalconsumer.StartLegacySharedPreFenceCompatibility {
		return binding.SerializationVersion == approvalencoding.LegacySerializationVersion
	}
	if descriptor.Workload == approvalconsumer.BillingWorkload {
		return binding.SerializationVersion == approvalencoding.ApprovalSerializationVersion
	}
	return descriptor.Workload == approvalconsumer.TokenWorkload &&
		(binding.SerializationVersion == approvalencoding.LegacySerializationVersion || binding.SerializationVersion == approvalencoding.ApprovalSerializationVersion)
}

func (consumer *TokenAuthorizationCommandConsumer) advance(ctx context.Context, delivery events.AuthorizationRawDelivery, binding tokenAuthorizationBinding, event approvalencoding.AuthorizationEvent, state tokenAuthorizationDeliveryState) error {
	switch state.State {
	case "TERMINAL_CONFIRMED":
		return delivery.DoubleAck(ctx)
	case "RESULT_CONFIRMED":
		if err := delivery.DoubleAck(ctx); err != nil {
			return err
		}
		terminal, err := consumer.recordState(ctx, binding, "terminal", state.Version)
		if err != nil || terminal.State != "TERMINAL_CONFIRMED" || terminal.Version <= state.Version || !validTokenAuthorizationState(terminal) {
			return errors.Join(errTokenAuthorizationAlreadyAcknowledged, err)
		}
		return nil
	case "READY":
	default:
		return errors.New("Token authorization delivery is not executable")
	}

	executor := consumer.executor
	active, err := executor.activation.Snapshot(ctx)
	if err != nil || active.State != "ACTIVE" || active.Epoch < 1 {
		return ErrApprovalResultUnconfirmed
	}
	selected := tokenEventFromAuthorization(event)
	path := "/internal/v1/commands/" + approvedCommandPaths[executor.commandType]
	lookup := path + "/result?approval_request_id=" + binding.ApprovalRequestID.String() + "&command_fingerprint=" + selected.CommandFingerprint
	var domain TokenCommandResult
	status, requestErr := executor.request(ctx, executor.tokenURL, http.MethodGet, lookup, nil, &domain)
	if requestErr != nil || status != http.StatusOK && status != http.StatusNotFound {
		return ErrApprovalResultUnconfirmed
	}
	found := status == http.StatusOK
	var projection ApprovalSnapshotProjection
	if found {
		projection.CriticalResourceVersions = selected.CriticalResourceVersions
		projection.FinancialSnapshot = selected.FinancialSnapshot
	} else {
		if !selected.ExecutionDeadline.After(time.Now()) {
			return ErrApprovalResultUnconfirmed
		}
		status, requestErr = executor.request(ctx, executor.tokenURL, http.MethodPost, path+"/precheck", ApprovalSnapshotRequest{
			CommandType: selected.CommandType, TargetResourceType: selected.TargetResourceType,
			TargetResourceID: selected.TargetResourceID, NormalizedCommand: selected.NormalizedCommand,
		}, &projection)
		if requestErr != nil || status != http.StatusOK || projection.CommandType != selected.CommandType || projection.TargetResourceType != selected.TargetResourceType || projection.TargetResourceID != selected.TargetResourceID || !validImpactFingerprint(projection.PrecheckFingerprint) {
			return ErrApprovalResultUnconfirmed
		}
	}

	lease, err := consumer.begin(ctx, binding, binding.EventID.String(), projection.CriticalResourceVersions, projection.FinancialSnapshot)
	if err != nil || !validTokenAuthorizationLease(selected, binding, lease) {
		return fmt.Errorf("%w: APPROVAL_LEASE_BINDING_INVALID", ErrApprovalResultUnconfirmed)
	}
	if !found && lease.FirstClaim {
		if !equalTokenApprovalJSONObjects(projection.CriticalResourceVersions, lease.CriticalResourceVersions) || !sameTokenApprovalFinancial(executor.commandType, projection.FinancialSnapshot, lease.FinancialSnapshot) {
			return fmt.Errorf("%w: APPROVAL_LEASE_FACTS_CHANGED", ErrApprovalResultUnconfirmed)
		}
		envelope := TokenCommandEnvelope{CommandType: lease.CommandType, Reason: lease.RequesterReason, TargetResourceType: lease.TargetResourceType, TargetResourceID: lease.TargetResourceID, ExpectedVersions: lease.CriticalResourceVersions, PrecheckFingerprint: projection.PrecheckFingerprint, Command: lease.NormalizedCommand}
		if _, _, _, _, err := normalizeTokenCommand(envelope, lease.ExecutionIdempotencyKey, lease.CommandFingerprint); err != nil {
			return fmt.Errorf("%w: APPROVAL_COMMAND_ENVELOPE_INVALID", ErrApprovalResultUnconfirmed)
		}
		current, readErr := executor.activation.Snapshot(ctx)
		if readErr != nil || current.State != "ACTIVE" || current.Epoch != active.Epoch {
			return fmt.Errorf("%w: APPROVAL_ACTIVATION_CHANGED", ErrApprovalResultUnconfirmed)
		}
		status, requestErr = executor.request(ctx, executor.tokenURL, http.MethodPost, path+"/execute", approvedTokenCommandBody{
			ActivationEpoch: active.Epoch, ApprovalRequestID: lease.ApprovalRequestID, RequesterUserID: lease.RequesterUserID,
			AuthorizationVersion: lease.RequesterAuthorizationVersion, CommandFingerprint: lease.CommandFingerprint,
			ExecutionIdempotencyKey: lease.ExecutionIdempotencyKey, CriticalResourceVersions: lease.CriticalResourceVersions,
			FinancialSnapshot: lease.FinancialSnapshot, Envelope: envelope,
			ProofSchemaVersion: selected.ProofSchemaVersion, CommandDigest: selected.CommandDigest,
			SnapshotDigest: selected.SnapshotDigest, DomainRevision: selected.DomainRevision,
			ProofDigest: selected.ProofDigest, ApprovalControlEpoch: selected.ApprovalControlEpoch,
		}, &domain)
		found = requestErr == nil && (status == http.StatusOK || status == http.StatusAccepted)
	}
	if !found {
		status, requestErr = executor.request(ctx, executor.tokenURL, http.MethodGet, lookup, nil, &domain)
		if requestErr != nil || status != http.StatusOK {
			return ErrApprovalResultUnconfirmed
		}
	}
	if !validTokenAuthorizationResult(domain, lease) {
		return ErrApprovalResultUnconfirmed
	}
	current, err := executor.activation.Snapshot(ctx)
	if err != nil || current.State != "ACTIVE" || current.Epoch != active.Epoch {
		return ErrApprovalResultUnconfirmed
	}
	completion, errorClass := "EXECUTED", ""
	if domain.Status != "SUCCEEDED" {
		completion, errorClass = "FAILED", "DOMAIN_EXECUTION_FAILED"
	}
	status, requestErr = executor.request(ctx, executor.accountURL, http.MethodPost,
		"/internal/v1/approval-requests/"+binding.ApprovalRequestID.String()+"/executions/complete",
		map[string]any{"job_run_id": binding.EventID.String(), "result": completion, "domain_result": map[string]any{"command_reference": domain.CommandReference, "status": domain.Status}, "ledger_references": map[string]any{}, "error_class": errorClass}, nil)
	if requestErr != nil || status != http.StatusNoContent {
		return ErrApprovalResultUnconfirmed
	}
	confirmed, err := consumer.recordState(ctx, binding, "result", state.Version)
	if err != nil || confirmed.State != "RESULT_CONFIRMED" || confirmed.Version <= state.Version || !validTokenAuthorizationState(confirmed) {
		return errors.New("Token authorization result was not persisted")
	}
	if err := delivery.DoubleAck(ctx); err != nil {
		return err
	}
	terminal, err := consumer.recordState(ctx, binding, "terminal", confirmed.Version)
	if err != nil || terminal.State != "TERMINAL_CONFIRMED" || terminal.Version <= confirmed.Version || !validTokenAuthorizationState(terminal) {
		return errors.Join(errTokenAuthorizationAlreadyAcknowledged, err)
	}
	return nil
}

func tokenEventFromAuthorization(event approvalencoding.AuthorizationEvent) tokenAuthorizedEvent {
	return tokenAuthorizedEvent{ApprovalRequestID: event.ApprovalRequestID, CommandType: event.CommandType,
		CommandFingerprint: event.CommandFingerprint, PolicyVersion: event.PolicyVersion, ExecutorIdentity: event.ExecutorIdentity,
		ExecutionDeadline: event.ExecutionDeadline, NormalizedCommand: event.NormalizedCommand, TargetResourceType: event.TargetResourceType,
		TargetResourceID: event.TargetResourceID, CriticalResourceVersions: event.CriticalResourceVersions, FinancialSnapshot: event.FinancialSnapshot,
		ProofSchemaVersion: event.ProofSchemaVersion, CommandDigest: event.CommandDigest,
		SnapshotDigest: event.SnapshotDigest, DomainRevision: event.DomainRevision,
		ProofDigest: event.ProofDigest, ApprovalControlEpoch: event.ApprovalControlEpoch}
}

func (consumer *TokenAuthorizationCommandConsumer) validBinding(binding tokenAuthorizationBinding) bool {
	descriptor, ok := approvalconsumer.LookupDurable(consumer.durable)
	return ok && descriptor.Workload == approvalconsumer.TokenWorkload && binding.EventID != uuid.Nil && binding.Durable == consumer.durable &&
		binding.Workload == approvalconsumer.TokenWorkload && binding.ApprovalRequestID != uuid.Nil && binding.ApprovedRequestVersion > 0 && binding.ApprovalEpoch > 0 &&
		(binding.SerializationVersion == approvalencoding.LegacySerializationVersion || binding.SerializationVersion == approvalencoding.ApprovalSerializationVersion) &&
		approvalconsumer.Match(descriptor, binding.CommandType, binding.PolicyVersion, binding.ExecutorIdentity)
}

func (consumer *TokenAuthorizationCommandConsumer) validDecoded(binding tokenAuthorizationBinding, decoded approvalencoding.DecodedAuthorizationEvent, raw []byte) bool {
	event := decoded.Payload
	if decoded.EventID != binding.EventID || event.EventID != binding.EventID || decoded.AggregateID != binding.ApprovalRequestID || event.ApprovalRequestID != binding.ApprovalRequestID ||
		decoded.AggregateVersion != binding.ApprovedRequestVersion || decoded.SerializationVersion != binding.SerializationVersion || event.CommandType != binding.CommandType || event.PolicyVersion != binding.PolicyVersion || event.ExecutorIdentity != binding.ExecutorIdentity {
		return false
	}
	var digestBytes []byte
	if binding.DigestKind == "DELIVERY_BYTES" && (binding.Provenance == "FROZEN_AT_APPROVAL" || binding.Provenance == "RECONSTRUCTED_FOR_DELIVERY") {
		digestBytes = raw
	} else if binding.DigestKind == "LEGACY_SEMANTIC" && decoded.SerializationVersion == approvalencoding.LegacySerializationVersion && binding.Provenance == "RECONSTRUCTED_AFTER_PUBLISH" {
		legacy := events.Event{ID: decoded.EventID, Type: approvalencoding.AuthorizationEventType, AggregateID: decoded.AggregateID, AggregateVersion: decoded.AggregateVersion, OccurredAt: decoded.OccurredAt, TraceID: decoded.TraceID, Payload: decoded.LegacyPayload}
		var err error
		digestBytes, err = legacy.Marshal()
		if err != nil {
			return false
		}
	} else {
		return false
	}
	digest := sha256.Sum256(digestBytes)
	return hex.EncodeToString(digest[:]) == binding.EventDigest
}

func validTokenAuthorizationState(state tokenAuthorizationDeliveryState) bool {
	if state.Version <= 0 || state.DomainAttemptCount != 0 || state.LastErrorCode != nil {
		return false
	}
	return state.State == "READY" || state.State == "RESULT_CONFIRMED" || state.State == "TERMINAL_CONFIRMED"
}

func validTokenAuthorizationLease(event tokenAuthorizedEvent, binding tokenAuthorizationBinding, lease tokenApprovalLease) bool {
	return validTokenApprovalLease(event, lease) && lease.AuthorizationEventID != nil && *lease.AuthorizationEventID == binding.EventID &&
		lease.AuthorizationDurable == binding.Durable && lease.AuthorizationApprovedVersion == binding.ApprovedRequestVersion &&
		lease.AuthorizationEventDigest == binding.EventDigest && lease.AuthorizationDigestKind == binding.DigestKind &&
		lease.AuthorizationSerialization == binding.SerializationVersion && lease.AuthorizationProvenance == binding.Provenance &&
		lease.AuthorizationApprovalEpoch == binding.ApprovalEpoch
}

func validTokenAuthorizationResult(domain TokenCommandResult, lease tokenApprovalLease) bool {
	return domain.CommandReference != uuid.Nil && domain.ApprovalRequestID != nil && *domain.ApprovalRequestID == lease.ApprovalRequestID &&
		domain.CommandType == lease.CommandType && domain.PolicyVersion == lease.PolicyVersion && domain.ActorUserID == lease.RequesterUserID &&
		domain.AuthorizationVersion == lease.RequesterAuthorizationVersion && domain.TargetResourceType == lease.TargetResourceType &&
		domain.TargetResourceID == lease.TargetResourceID && domain.CompletedAt != nil && !domain.CompletedAt.IsZero() &&
		(domain.Status == "SUCCEEDED" || domain.Status == "FAILED" || domain.Status == "REJECTED")
}

func (consumer *TokenAuthorizationCommandConsumer) resolveBinding(ctx context.Context, eventID uuid.UUID) (tokenAuthorizationBinding, error) {
	var binding tokenAuthorizationBinding
	status, err := consumer.executor.request(ctx, consumer.executor.accountURL, http.MethodGet,
		"/internal/v1/approval-authorization/events/"+eventID.String()+"/binding?durable="+url.QueryEscape(consumer.durable), nil, &binding)
	if err != nil || status != http.StatusOK {
		return tokenAuthorizationBinding{}, errors.New("authorization binding unavailable")
	}
	return binding, nil
}

func (consumer *TokenAuthorizationCommandConsumer) resolveRoute(ctx context.Context, eventID uuid.UUID) (tokenAuthorizationRoute, error) {
	var route tokenAuthorizationRoute
	status, err := consumer.executor.request(ctx, consumer.executor.accountURL, http.MethodGet,
		"/internal/v1/approval-authorization/events/"+eventID.String()+"/route", nil, &route)
	if err != nil || status != http.StatusOK {
		return tokenAuthorizationRoute{}, errors.New("authorization route unavailable")
	}
	return route, nil
}

func (consumer *TokenAuthorizationCommandConsumer) observe(ctx context.Context, binding tokenAuthorizationBinding, stream string, sequence approvalconsumer.Sequence) (tokenAuthorizationDeliveryState, error) {
	payload := map[string]any{"approval_request_id": binding.ApprovalRequestID, "approved_request_version": binding.ApprovedRequestVersion,
		"approval_epoch": binding.ApprovalEpoch, "serialization_version": binding.SerializationVersion, "provenance": binding.Provenance,
		"event_digest": binding.EventDigest, "digest_kind": binding.DigestKind, "command_type": binding.CommandType,
		"policy_version": binding.PolicyVersion, "executor_identity": binding.ExecutorIdentity, "stream": stream, "stream_sequence": sequence}
	return consumer.stateRequest(ctx, binding, "observe", payload)
}

func (consumer *TokenAuthorizationCommandConsumer) begin(ctx context.Context, binding tokenAuthorizationBinding, jobRunID string, versions, financial json.RawMessage) (tokenApprovalLease, error) {
	var lease tokenApprovalLease
	payload := map[string]any{"job_run_id": jobRunID, "observed_versions": versions, "observed_financial_snapshot": financial, "authorization_binding": binding}
	status, err := consumer.executor.request(ctx, consumer.executor.accountURL, http.MethodPost,
		"/internal/v1/approval-requests/"+binding.ApprovalRequestID.String()+"/executions", payload, &lease)
	if err != nil || status != http.StatusOK {
		return tokenApprovalLease{}, errors.New("authorization begin unavailable")
	}
	return lease, nil
}

func (consumer *TokenAuthorizationCommandConsumer) recordState(ctx context.Context, binding tokenAuthorizationBinding, action string, version int64) (tokenAuthorizationDeliveryState, error) {
	return consumer.stateRequest(ctx, binding, action, map[string]any{"expected_version": version})
}

func (consumer *TokenAuthorizationCommandConsumer) stateRequest(ctx context.Context, binding tokenAuthorizationBinding, action string, payload any) (tokenAuthorizationDeliveryState, error) {
	var state tokenAuthorizationDeliveryState
	path := fmt.Sprintf("/internal/v1/approval-authorization/events/%s/deliveries/%s/%s", binding.EventID, url.PathEscape(binding.Durable), action)
	status, err := consumer.executor.request(ctx, consumer.executor.accountURL, http.MethodPost, path, payload, &state)
	if err != nil || status != http.StatusOK {
		return tokenAuthorizationDeliveryState{}, errors.New("authorization state unavailable")
	}
	return state, nil
}

func (consumer *TokenAuthorizationCommandConsumer) nak(delivery events.AuthorizationRawDelivery) {
	exponent := delivery.Metadata().NumDelivered
	if exponent > tokenAuthorizationRetryMaximumExponent {
		exponent = tokenAuthorizationRetryMaximumExponent
	}
	if exponent == 0 {
		exponent = 1
	}
	if err := delivery.NakWithDelay(time.Second * time.Duration(uint64(1)<<exponent)); err != nil {
		slog.Error("TOKEN_APPROVAL_AUTHORIZATION_NAK_FAILED")
	}
}
