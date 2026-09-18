package billing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
)

const authorizationRetryMaximumExponent = 9

var errAuthorizationDeliveryAlreadyAcknowledged = errors.New("authorization delivery was acknowledged before terminal state persistence")

type approvalAuthorizationAccount interface {
	ResolveAuthorizationRoute(context.Context, uuid.UUID) (ApprovalAuthorizationRoute, error)
	ResolveAuthorizationBinding(context.Context, uuid.UUID, string) (ApprovalAuthorizationBinding, error)
	ObserveAuthorizationDelivery(context.Context, ApprovalAuthorizationBinding, string, approvalconsumer.Sequence) (ApprovalAuthorizationDeliveryState, error)
	AuthorizationDeliveryState(context.Context, ApprovalAuthorizationBinding) (ApprovalAuthorizationDeliveryState, error)
	BeginAuthorized(context.Context, ApprovalAuthorizationBinding, string, json.RawMessage, json.RawMessage) (ApprovedCommand, error)
	Complete(context.Context, uuid.UUID, string, string, json.RawMessage, json.RawMessage, string) error
	GrantAuthorizationAttempt(context.Context, ApprovalAuthorizationBinding, int64) (ApprovalAuthorizationDeliveryState, error)
	RecordAuthorizationFailure(context.Context, ApprovalAuthorizationBinding, int64, string) (ApprovalAuthorizationDeliveryState, error)
	RecordAuthorizationResult(context.Context, ApprovalAuthorizationBinding, int64) (ApprovalAuthorizationDeliveryState, error)
	RecordAuthorizationDLQ(context.Context, ApprovalAuthorizationBinding, int64, string, approvalconsumer.Sequence) (ApprovalAuthorizationDeliveryState, error)
	RecordAuthorizationTerminal(context.Context, ApprovalAuthorizationBinding, int64) (ApprovalAuthorizationDeliveryState, error)
}

type authorizationDomainResult struct {
	DomainResult     json.RawMessage
	LedgerReferences json.RawMessage
}

type authorizationDomainAttempt struct {
	Result            authorizationDomainResult
	StableFailureCode string
	FailureIsDefinite bool
}

type approvalAuthorizationDomain interface {
	LookupAuthorizationResult(context.Context, ApprovalAuthorizationBinding, approvalencoding.AuthorizationEvent) (authorizationDomainResult, bool, error)
	ExecuteAuthorizationAttempt(context.Context, ApprovalAuthorizationBinding, approvalencoding.AuthorizationEvent, ApprovedCommand) authorizationDomainAttempt
}

type AuthorizationCommandConsumer struct {
	account  approvalAuthorizationAccount
	domain   approvalAuthorizationDomain
	durable  string
	workload string
}

func NewAuthorizationCommandConsumer(service *Service, account approvalAuthorizationAccount, durable string) (*AuthorizationCommandConsumer, error) {
	descriptor, ok := approvalconsumer.LookupDurable(durable)
	if !ok || descriptor.Workload != approvalconsumer.BillingWorkload || service == nil || account == nil {
		return nil, errors.New("invalid billing authorization consumer configuration")
	}
	return &AuthorizationCommandConsumer{account: account, domain: newBillingAuthorizationDomain(service), durable: durable, workload: descriptor.Workload}, nil
}

func (consumer *AuthorizationCommandConsumer) Handle(ctx context.Context, delivery events.AuthorizationRawDelivery) {
	if consumer == nil || consumer.account == nil || consumer.domain == nil || delivery == nil {
		return
	}
	if err := consumer.handle(ctx, delivery); err != nil {
		slog.Error("BILLING_APPROVAL_AUTHORIZATION_DELIVERY_PENDING")
		if !errors.Is(err, errAuthorizationDeliveryAlreadyAcknowledged) {
			consumer.nak(delivery)
		}
	}
}

func (consumer *AuthorizationCommandConsumer) handle(ctx context.Context, delivery events.AuthorizationRawDelivery) error {
	eventID, err := uuid.Parse(delivery.MessageID())
	if err != nil || eventID == uuid.Nil || eventID.String() != delivery.MessageID() {
		return errors.New("invalid authorization message identity")
	}
	route, err := consumer.account.ResolveAuthorizationRoute(ctx, eventID)
	if err != nil || !validAuthorizationRoute(route) {
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
	if route.CanonicalWorkload != consumer.workload || route.CanonicalDurable != consumer.durable {
		return delivery.DoubleAck(ctx)
	}
	binding, err := consumer.account.ResolveAuthorizationBinding(ctx, eventID, consumer.durable)
	if err != nil || !consumer.validBinding(binding) || binding != routeBinding {
		return errors.New("authorization binding unavailable")
	}
	state, err := consumer.account.ObserveAuthorizationDelivery(ctx, binding, metadata.Stream, sequence)
	if err != nil || !validAuthorizationDeliveryState(state) {
		return errors.New("authorization delivery observation unavailable")
	}
	return consumer.advance(ctx, delivery, binding, decoded.Payload, state)
}

func validAuthorizationRoute(route ApprovalAuthorizationRoute) bool {
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

func (consumer *AuthorizationCommandConsumer) advance(ctx context.Context, delivery events.AuthorizationRawDelivery, binding ApprovalAuthorizationBinding, event approvalencoding.AuthorizationEvent, state ApprovalAuthorizationDeliveryState) error {
	switch state.State {
	case "TERMINAL_CONFIRMED":
		return delivery.DoubleAck(ctx)
	case "RESULT_CONFIRMED", "DLQ_PUBLISHED_ACK_PENDING":
		if err := delivery.DoubleAck(ctx); err != nil {
			return err
		}
		terminal, err := consumer.account.RecordAuthorizationTerminal(ctx, binding, state.Version)
		if err != nil || terminal.State != "TERMINAL_CONFIRMED" || terminal.Version <= state.Version || !validAuthorizationDeliveryState(terminal) {
			return errors.Join(errAuthorizationDeliveryAlreadyAcknowledged, err)
		}
		return nil
	}

	result, found, err := consumer.domain.LookupAuthorizationResult(ctx, binding, event)
	if err != nil {
		return err
	}
	if found {
		return consumer.finishResult(ctx, delivery, binding, state, result)
	}
	if state.State == "DOMAIN_INFLIGHT" {
		current, readErr := consumer.account.AuthorizationDeliveryState(ctx, binding)
		if readErr != nil || !validAuthorizationDeliveryState(current) {
			return errors.New("authorization domain outcome remains unknown")
		}
		if current.State == "RESULT_CONFIRMED" || current.State == "DLQ_PUBLISHED_ACK_PENDING" || current.State == "TERMINAL_CONFIRMED" {
			return consumer.advance(ctx, delivery, binding, event, current)
		}
		if current.State != "DOMAIN_INFLIGHT" {
			return errors.New("authorization delivery changed concurrently")
		}
		if current.DomainAttemptCount == approvalconsumer.MaxDomainAttempts && current.LastErrorCode != nil && validAuthorizationFailureCode(*current.LastErrorCode) {
			return consumer.publishDeadLetterAndAck(ctx, delivery, binding, current)
		}
		if current.LastErrorCode == nil && current.ExecutionStatus != nil && *current.ExecutionStatus == "FAILED" && current.ExecutionErrorClass != nil && validAuthorizationFailureCode(*current.ExecutionErrorClass) {
			failed, failureErr := consumer.account.RecordAuthorizationFailure(ctx, binding, current.Version, *current.ExecutionErrorClass)
			if failureErr != nil || !validAuthorizationDeliveryState(failed) || failed.Version <= current.Version {
				return errors.New("authorization completed failure remains unrecorded")
			}
			if failed.DomainAttemptCount == approvalconsumer.MaxDomainAttempts && failed.State == "DOMAIN_INFLIGHT" {
				return consumer.publishDeadLetterAndAck(ctx, delivery, binding, failed)
			}
			return errors.New("authorization completed failure was recovered")
		}
		// Absence after a previous call is never evidence that the call did not
		// commit or is not still running. Do not spend another attempt.
		return errors.New("authorization domain outcome remains unknown")
	}
	if state.State != "READY" || state.DomainAttemptCount >= approvalconsumer.MaxDomainAttempts {
		return errors.New("authorization delivery is not executable")
	}
	// A proof-era grant is single-shot until a separately reviewed recovery
	// contract exists. This local gate also covers a mixed-version Account
	// deployment that still exposes an old READY failure row.
	if binding.SerializationVersion == approvalencoding.ApprovalSerializationVersion &&
		(state.DomainAttemptCount != 0 || state.LastErrorCode != nil) {
		return errors.New("proof-era authorization requires result-only recovery")
	}
	jobRunID := binding.EventID.String()
	// The proof wire encodes every int64 as a canonical decimal string while
	// Account and the domain handlers still own the JSON-number contract, so the
	// observed values handed to Account and compared against the approved command
	// must be normalised back to the domain form (approvalencoding's closed
	// conversion, never a heuristic re-shape).
	executionEvent := event
	if binding.SerializationVersion == approvalencoding.ApprovalSerializationVersion {
		legacy, normaliseErr := approvalencoding.LegacyExecutionInput(approvalencoding.DecodedAuthorizationEvent{
			EventID: event.EventID, AggregateID: event.ApprovalRequestID, AggregateVersion: binding.ApprovedRequestVersion,
			SerializationVersion: binding.SerializationVersion, Payload: event,
		})
		if normaliseErr != nil {
			return errors.New("authorization wire cannot be normalised")
		}
		executionEvent.NormalizedCommand = legacy.NormalizedCommand
		executionEvent.CriticalResourceVersions = legacy.CriticalResourceVersions
		executionEvent.FinancialSnapshot = legacy.FinancialSnapshot
	}
	// Proof-era Billing wire consumes the single domain attempt before the
	// Account begin: the Account contract only admits the first Billing begin
	// from DOMAIN_INFLIGHT, and a crash after the grant must still consume the
	// attempt. The legacy wire keeps its historical begin-then-grant order.
	var granted ApprovalAuthorizationDeliveryState
	if binding.SerializationVersion == approvalencoding.ApprovalSerializationVersion {
		granted, err = consumer.account.GrantAuthorizationAttempt(ctx, binding, state.Version)
		if err != nil || granted.State != "DOMAIN_INFLIGHT" || granted.DomainAttemptCount != state.DomainAttemptCount+1 || granted.Version <= state.Version {
			return errors.New("authorization attempt was not granted")
		}
	}
	command, err := consumer.account.BeginAuthorized(ctx, binding, jobRunID, executionEvent.CriticalResourceVersions, executionEvent.FinancialSnapshot)
	if err != nil || !validApprovedAuthorizationCommand(command, binding, executionEvent) {
		return errors.New("authorization begin unavailable")
	}
	if binding.SerializationVersion != approvalencoding.ApprovalSerializationVersion {
		granted, err = consumer.account.GrantAuthorizationAttempt(ctx, binding, state.Version)
		if err != nil || granted.State != "DOMAIN_INFLIGHT" || granted.DomainAttemptCount != state.DomainAttemptCount+1 || granted.Version <= state.Version {
			return errors.New("authorization attempt was not granted")
		}
	}
	attempt := consumer.domain.ExecuteAuthorizationAttempt(ctx, binding, executionEvent, command)
	if len(attempt.Result.DomainResult) != 0 {
		if err := consumer.account.Complete(ctx, binding.ApprovalRequestID, jobRunID, "EXECUTED", attempt.Result.DomainResult, jsonOrEmpty(attempt.Result.LedgerReferences), ""); err != nil {
			return err
		}
		return consumer.persistResultAndAck(ctx, delivery, binding, granted, attempt.Result)
	}
	if !attempt.FailureIsDefinite || !validAuthorizationFailureCode(attempt.StableFailureCode) {
		return errors.New("authorization domain outcome remains unknown")
	}
	if err := consumer.account.Complete(ctx, binding.ApprovalRequestID, jobRunID, "FAILED", json.RawMessage(`{}`), json.RawMessage(`{}`), attempt.StableFailureCode); err != nil {
		return err
	}
	failed, err := consumer.account.RecordAuthorizationFailure(ctx, binding, granted.Version, attempt.StableFailureCode)
	if err != nil || failed.Version <= granted.Version || (failed.State != "READY" && failed.State != "DOMAIN_INFLIGHT") {
		return errors.New("authorization failure was not persisted")
	}
	if failed.DomainAttemptCount == approvalconsumer.MaxDomainAttempts && failed.State == "DOMAIN_INFLIGHT" {
		return consumer.publishDeadLetterAndAck(ctx, delivery, binding, failed)
	}
	return errors.New("authorization attempt failed")
}

func (consumer *AuthorizationCommandConsumer) publishDeadLetterAndAck(ctx context.Context, delivery events.AuthorizationRawDelivery, binding ApprovalAuthorizationBinding, state ApprovalAuthorizationDeliveryState) error {
	ack, err := delivery.PublishDeadLetter(ctx)
	if err != nil || ack.Stream != "ACCOUNT_PLATFORM" || ack.StreamSequence == 0 {
		return errors.New("authorization DLQ publish remains unconfirmed")
	}
	pending, err := consumer.account.RecordAuthorizationDLQ(ctx, binding, state.Version, binding.EventID.String()+":"+binding.Durable+":dead-letter", approvalconsumer.Sequence(ack.StreamSequence))
	if err != nil || pending.State != "DLQ_PUBLISHED_ACK_PENDING" || pending.Version <= state.Version {
		return errors.New("authorization DLQ acknowledgement was not persisted")
	}
	if err := delivery.DoubleAck(ctx); err != nil {
		return err
	}
	terminal, err := consumer.account.RecordAuthorizationTerminal(ctx, binding, pending.Version)
	if err != nil || terminal.State != "TERMINAL_CONFIRMED" || terminal.Version <= pending.Version || !validAuthorizationDeliveryState(terminal) {
		return errors.Join(errAuthorizationDeliveryAlreadyAcknowledged, err)
	}
	return nil
}

func (consumer *AuthorizationCommandConsumer) finishResult(ctx context.Context, delivery events.AuthorizationRawDelivery, binding ApprovalAuthorizationBinding, state ApprovalAuthorizationDeliveryState, result authorizationDomainResult) error {
	if err := consumer.account.Complete(ctx, binding.ApprovalRequestID, binding.EventID.String(), "EXECUTED", result.DomainResult, jsonOrEmpty(result.LedgerReferences), ""); err != nil {
		return err
	}
	return consumer.persistResultAndAck(ctx, delivery, binding, state, result)
}

func (consumer *AuthorizationCommandConsumer) persistResultAndAck(ctx context.Context, delivery events.AuthorizationRawDelivery, binding ApprovalAuthorizationBinding, state ApprovalAuthorizationDeliveryState, _ authorizationDomainResult) error {
	confirmed, err := consumer.account.RecordAuthorizationResult(ctx, binding, state.Version)
	if err != nil || confirmed.State != "RESULT_CONFIRMED" || confirmed.Version <= state.Version {
		return errors.New("authorization result was not persisted")
	}
	if err := delivery.DoubleAck(ctx); err != nil {
		return err
	}
	terminal, err := consumer.account.RecordAuthorizationTerminal(ctx, binding, confirmed.Version)
	if err != nil || terminal.State != "TERMINAL_CONFIRMED" || terminal.Version <= confirmed.Version || !validAuthorizationDeliveryState(terminal) {
		return errors.Join(errAuthorizationDeliveryAlreadyAcknowledged, err)
	}
	return nil
}

func (consumer *AuthorizationCommandConsumer) nak(delivery events.AuthorizationRawDelivery) {
	metadata := delivery.Metadata()
	exponent := metadata.NumDelivered
	if exponent > authorizationRetryMaximumExponent {
		exponent = authorizationRetryMaximumExponent
	}
	if exponent == 0 {
		exponent = 1
	}
	if err := delivery.NakWithDelay(time.Second * time.Duration(uint64(1)<<exponent)); err != nil {
		slog.Error("BILLING_APPROVAL_AUTHORIZATION_NAK_FAILED")
	}
}

func (consumer *AuthorizationCommandConsumer) validBinding(binding ApprovalAuthorizationBinding) bool {
	descriptor, ok := approvalconsumer.LookupDurable(consumer.durable)
	if !ok || binding.EventID == uuid.Nil || binding.Durable != consumer.durable || binding.Workload != consumer.workload || binding.ApprovalRequestID == uuid.Nil || binding.ExecutorIdentity != consumer.workload || binding.CommandType == "" || binding.PolicyVersion <= 0 {
		return false
	}
	if descriptor.StartSource == approvalconsumer.StartLegacySharedPreFenceCompatibility {
		if binding.SerializationVersion != approvalencoding.LegacySerializationVersion {
			return false
		}
		for _, candidate := range approvalconsumer.BillingCommands() {
			if candidate.CommandType == binding.CommandType && candidate.PolicyVersion == binding.PolicyVersion && candidate.ExecutorIdentity == binding.ExecutorIdentity {
				return true
			}
		}
		return false
	}
	return binding.SerializationVersion == approvalencoding.ApprovalSerializationVersion &&
		approvalconsumer.Match(descriptor, binding.CommandType, binding.PolicyVersion, binding.ExecutorIdentity)
}

func (consumer *AuthorizationCommandConsumer) validDecoded(binding ApprovalAuthorizationBinding, decoded approvalencoding.DecodedAuthorizationEvent, raw []byte) bool {
	event := decoded.Payload
	if decoded.EventID != binding.EventID || event.EventID != binding.EventID || decoded.AggregateID != binding.ApprovalRequestID || event.ApprovalRequestID != binding.ApprovalRequestID ||
		decoded.AggregateVersion != binding.ApprovedRequestVersion || decoded.SerializationVersion != binding.SerializationVersion || event.CommandType != binding.CommandType || event.PolicyVersion != binding.PolicyVersion || event.ExecutorIdentity != binding.ExecutorIdentity {
		return false
	}
	if !validAuthorizationCommandTarget(event) {
		return false
	}
	var digestBytes []byte
	if binding.DigestKind == "DELIVERY_BYTES" {
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

func validApprovedAuthorizationCommand(command ApprovedCommand, binding ApprovalAuthorizationBinding, event approvalencoding.AuthorizationEvent) bool {
	return command.ApprovalRequestID == binding.ApprovalRequestID && command.CommandType == binding.CommandType && command.PolicyVersion == binding.PolicyVersion &&
		command.CommandFingerprint == event.CommandFingerprint && command.TargetResourceType == event.TargetResourceType && command.TargetResourceID == event.TargetResourceID &&
		command.RequesterUserID != uuid.Nil && command.ApproverUserID != uuid.Nil && command.RequesterUserID != command.ApproverUserID &&
		strings.TrimSpace(command.DecisionReason) != "" && strings.TrimSpace(command.ExecutionIdempotencyKey) != "" && command.ExecutorIdentity == binding.ExecutorIdentity && command.Status == "EXECUTING" &&
		command.AuthorizationEventID != nil && *command.AuthorizationEventID == binding.EventID && command.AuthorizationDurable == binding.Durable &&
		command.AuthorizationApprovedVersion == binding.ApprovedRequestVersion && command.AuthorizationEventDigest == binding.EventDigest && command.AuthorizationDigestKind == binding.DigestKind &&
		command.AuthorizationSerialization == binding.SerializationVersion && command.AuthorizationProvenance == binding.Provenance && command.AuthorizationApprovalEpoch == binding.ApprovalEpoch &&
		sameRawJSON(command.NormalizedCommand, event.NormalizedCommand) && sameRawJSON(command.CriticalResourceVersions, event.CriticalResourceVersions) && sameRawJSON(command.FinancialSnapshot, event.FinancialSnapshot)
}

func validAuthorizationDeliveryState(state ApprovalAuthorizationDeliveryState) bool {
	if state.Version <= 0 || state.DomainAttemptCount < 0 || state.DomainAttemptCount > approvalconsumer.MaxDomainAttempts {
		return false
	}
	switch state.State {
	case "READY", "DOMAIN_INFLIGHT", "RESULT_CONFIRMED", "DLQ_PUBLISHED_ACK_PENDING", "TERMINAL_CONFIRMED":
		return true
	default:
		return false
	}
}

func validAuthorizationFailureCode(code string) bool {
	if code == "" || len(code) > 120 || code != strings.TrimSpace(code) {
		return false
	}
	for index, current := range code {
		if (index == 0 && (current < 'A' || current > 'Z')) || (index > 0 && (current < 'A' || current > 'Z') && (current < '0' || current > '9') && current != '_') {
			return false
		}
	}
	for _, sensitive := range []string{"TOKEN", "KEY", "SECRET", "PASSWORD", "CREDENTIAL"} {
		if strings.Contains(code, sensitive) {
			return false
		}
	}
	return true
}

func sameRawJSON(left, right json.RawMessage) bool {
	var leftValue, rightValue any
	if !decodeSingleJSONValue(left, &leftValue) || !decodeSingleJSONValue(right, &rightValue) {
		return false
	}
	return equalJSONValue(leftValue, rightValue)
}

func decodeSingleJSONValue(raw json.RawMessage, destination *any) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		return false
	}
	var trailing any
	return errors.Is(decoder.Decode(&trailing), io.EOF)
}

func equalJSONValue(left, right any) bool {
	switch typedLeft := left.(type) {
	case nil:
		return right == nil
	case bool:
		typedRight, ok := right.(bool)
		return ok && typedLeft == typedRight
	case string:
		typedRight, ok := right.(string)
		return ok && typedLeft == typedRight
	case json.Number:
		typedRight, ok := right.(json.Number)
		if !ok {
			return false
		}
		leftNumber, leftOK := new(big.Rat).SetString(typedLeft.String())
		rightNumber, rightOK := new(big.Rat).SetString(typedRight.String())
		return leftOK && rightOK && leftNumber.Cmp(rightNumber) == 0
	case []any:
		typedRight, ok := right.([]any)
		if !ok || len(typedLeft) != len(typedRight) {
			return false
		}
		for index := range typedLeft {
			if !equalJSONValue(typedLeft[index], typedRight[index]) {
				return false
			}
		}
		return true
	case map[string]any:
		typedRight, ok := right.(map[string]any)
		if !ok || len(typedLeft) != len(typedRight) {
			return false
		}
		for key, value := range typedLeft {
			other, exists := typedRight[key]
			if !exists || !equalJSONValue(value, other) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func jsonOrEmpty(value json.RawMessage) json.RawMessage {
	if len(value) == 0 {
		return json.RawMessage(`{}`)
	}
	return value
}
