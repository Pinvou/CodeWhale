package account

import (
	"context"
	"errors"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrApprovalConsumerBindingUnavailable = errors.New("approval consumer binding unavailable")
	ErrApprovalConsumerBindingInvalid     = errors.New("approval consumer binding invalid")
)

type ApprovalAuthorizationBinding struct {
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

// ApprovalAuthorizationRoute authenticates a shared-subject message and names
// its single registry-owned target. It is read-only and does not confer the
// target-specific Observe/Begin/CAS authority represented by a binding.
type ApprovalAuthorizationRoute struct {
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

type ApprovalDeliveryObservation struct {
	Binding        ApprovalAuthorizationBinding `json:"binding"`
	Stream         string                       `json:"stream"`
	StreamSequence approvalconsumer.Sequence    `json:"stream_sequence"`
}

type ApprovalDeliveryState struct {
	DomainAttemptCount  int16   `json:"domain_attempt_count"`
	State               string  `json:"state"`
	Version             int64   `json:"version"`
	LastErrorCode       *string `json:"last_error_code,omitempty"`
	ExecutionStatus     *string `json:"execution_status,omitempty"`
	ExecutionErrorClass *string `json:"execution_error_class,omitempty"`
}

func (r *PostgresRepository) ApprovalAuthorizationDeliveryState(ctx context.Context, eventID uuid.UUID, durable, workload string) (ApprovalDeliveryState, error) {
	if !validApprovalAuthorizationLookup(eventID, durable, workload) {
		return ApprovalDeliveryState{}, ErrApprovalConsumerBindingInvalid
	}
	var state ApprovalDeliveryState
	err := r.pool.QueryRow(ctx, `SELECT domain_attempt_count,state,version,last_error_code,execution_status,execution_error_class
FROM public.read_approval_authorization_delivery_state($1,$2,$3)`, eventID, durable, workload).Scan(
		&state.DomainAttemptCount, &state.State, &state.Version, &state.LastErrorCode, &state.ExecutionStatus, &state.ExecutionErrorClass,
	)
	if err != nil {
		return ApprovalDeliveryState{}, approvalConsumerError(ctx, err)
	}
	return state, nil
}

func (r *PostgresRepository) ResolveApprovalAuthorizationBinding(ctx context.Context, eventID uuid.UUID, durable, workload string) (ApprovalAuthorizationBinding, error) {
	binding := ApprovalAuthorizationBinding{EventID: eventID, Durable: durable, Workload: workload}
	if !validApprovalAuthorizationLookup(binding.EventID, binding.Durable, binding.Workload) {
		return ApprovalAuthorizationBinding{}, ErrApprovalConsumerBindingInvalid
	}
	err := r.pool.QueryRow(ctx, `SELECT approval_request_id,approved_request_version,approval_epoch,serialization_version,
provenance,event_digest,digest_kind,command_type,policy_version,executor_identity
FROM public.resolve_approval_authorization_binding($1,$2,$3)`, binding.EventID, binding.Durable, binding.Workload).Scan(
		&binding.ApprovalRequestID, &binding.ApprovedRequestVersion, &binding.ApprovalEpoch,
		&binding.SerializationVersion, &binding.Provenance, &binding.EventDigest, &binding.DigestKind,
		&binding.CommandType, &binding.PolicyVersion, &binding.ExecutorIdentity,
	)
	if err != nil {
		return ApprovalAuthorizationBinding{}, approvalConsumerError(ctx, err)
	}
	if !validApprovalAuthorizationBinding(binding) {
		return ApprovalAuthorizationBinding{}, ErrApprovalConsumerBindingInvalid
	}
	return binding, nil
}

func (r *PostgresRepository) ResolveApprovalAuthorizationRoute(ctx context.Context, eventID uuid.UUID, callerWorkload string) (ApprovalAuthorizationRoute, error) {
	route := ApprovalAuthorizationRoute{EventID: eventID}
	if eventID == uuid.Nil || callerWorkload != "billing-command-executor" && callerWorkload != "token-command-executor" {
		return ApprovalAuthorizationRoute{}, ErrApprovalConsumerBindingInvalid
	}
	err := r.pool.QueryRow(ctx, `SELECT approval_request_id,approved_request_version,approval_epoch,serialization_version,
provenance,event_digest,digest_kind,command_type,policy_version,executor_identity,canonical_workload,canonical_durable
FROM public.resolve_approval_authorization_route($1,$2)`, eventID, callerWorkload).Scan(
		&route.ApprovalRequestID, &route.ApprovedRequestVersion, &route.ApprovalEpoch, &route.SerializationVersion,
		&route.Provenance, &route.EventDigest, &route.DigestKind, &route.CommandType, &route.PolicyVersion,
		&route.ExecutorIdentity, &route.CanonicalWorkload, &route.CanonicalDurable,
	)
	if err != nil {
		return ApprovalAuthorizationRoute{}, approvalConsumerError(ctx, err)
	}
	if !validApprovalAuthorizationRoute(route) {
		return ApprovalAuthorizationRoute{}, ErrApprovalConsumerBindingInvalid
	}
	return route, nil
}

func (r *PostgresRepository) ObserveApprovalAuthorizationDelivery(ctx context.Context, observation ApprovalDeliveryObservation) (ApprovalDeliveryState, error) {
	binding := observation.Binding
	if !validApprovalAuthorizationBinding(binding) || observation.Stream != "ACCOUNT_PLATFORM" || observation.StreamSequence == 0 {
		return ApprovalDeliveryState{}, ErrApprovalConsumerBindingInvalid
	}
	return scanApprovalDeliveryState(ctx, r.pool.QueryRow(ctx, `SELECT domain_attempt_count,state,version
FROM public.observe_approval_authorization_delivery($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::NUMERIC,$12,$13,$14)`,
		binding.EventID, binding.Durable, binding.Workload, binding.EventDigest, binding.DigestKind,
		binding.SerializationVersion, binding.Provenance, binding.ApprovedRequestVersion, binding.ApprovalEpoch,
		observation.Stream, observation.StreamSequence.String(), binding.CommandType, binding.PolicyVersion, binding.ExecutorIdentity))
}

func (r *PostgresRepository) GrantApprovalAuthorizationDomainAttempt(ctx context.Context, eventID uuid.UUID, durable, workload string, expectedVersion int64) (ApprovalDeliveryState, error) {
	if !validApprovalAuthorizationLookup(eventID, durable, workload) || expectedVersion <= 0 {
		return ApprovalDeliveryState{}, ErrApprovalConsumerBindingInvalid
	}
	return scanApprovalDeliveryState(ctx, r.pool.QueryRow(ctx, `SELECT domain_attempt_count,state,version
FROM public.grant_approval_authorization_domain_attempt($1,$2,$3,$4)`, eventID, durable, workload, expectedVersion))
}

func (r *PostgresRepository) RecordApprovalAuthorizationDomainFailure(ctx context.Context, eventID uuid.UUID, durable, workload string, expectedVersion int64, errorCode string) (ApprovalDeliveryState, error) {
	if !validApprovalAuthorizationLookup(eventID, durable, workload) || expectedVersion <= 0 || !validStableExecutionErrorCode(errorCode) {
		return ApprovalDeliveryState{}, ErrApprovalConsumerBindingInvalid
	}
	return scanApprovalDeliveryState(ctx, r.pool.QueryRow(ctx, `SELECT domain_attempt_count,state,version
FROM public.record_approval_authorization_domain_failure($1,$2,$3,$4,$5)`, eventID, durable, workload, expectedVersion, errorCode))
}

func (r *PostgresRepository) RecordApprovalAuthorizationResult(ctx context.Context, eventID uuid.UUID, durable, workload string, expectedVersion int64) (ApprovalDeliveryState, error) {
	if !validApprovalAuthorizationLookup(eventID, durable, workload) || expectedVersion <= 0 {
		return ApprovalDeliveryState{}, ErrApprovalConsumerBindingInvalid
	}
	return scanApprovalDeliveryState(ctx, r.pool.QueryRow(ctx, `SELECT domain_attempt_count,state,version
FROM public.record_approval_authorization_result($1,$2,$3,$4)`, eventID, durable, workload, expectedVersion))
}

func (r *PostgresRepository) RecordApprovalAuthorizationTerminal(ctx context.Context, eventID uuid.UUID, durable, workload string, expectedVersion int64) (ApprovalDeliveryState, error) {
	if !validApprovalAuthorizationLookup(eventID, durable, workload) || expectedVersion <= 0 {
		return ApprovalDeliveryState{}, ErrApprovalConsumerBindingInvalid
	}
	return scanApprovalDeliveryState(ctx, r.pool.QueryRow(ctx, `SELECT domain_attempt_count,state,version
FROM public.record_approval_authorization_terminal($1,$2,$3,$4)`, eventID, durable, workload, expectedVersion))
}

func (r *PostgresRepository) RecordApprovalAuthorizationDLQPubAck(ctx context.Context, eventID uuid.UUID, durable, workload string, expectedVersion int64, messageID string, streamSequence approvalconsumer.Sequence) (ApprovalDeliveryState, error) {
	if !validApprovalAuthorizationLookup(eventID, durable, workload) || expectedVersion <= 0 || streamSequence == 0 ||
		!boundedApprovalConsumerString(messageID, 240) || messageID != eventID.String()+":"+durable+":dead-letter" {
		return ApprovalDeliveryState{}, ErrApprovalConsumerBindingInvalid
	}
	return scanApprovalDeliveryState(ctx, r.pool.QueryRow(ctx, `SELECT domain_attempt_count,state,version
FROM public.record_approval_authorization_dlq_puback($1,$2,$3,$4,$5,$6::NUMERIC)`, eventID, durable, workload, expectedVersion, messageID, streamSequence.String()))
}

func validateApprovalAuthorizationBegin(ctx context.Context, tx pgx.Tx, binding ApprovalAuthorizationBinding) error {
	if !validApprovalAuthorizationBinding(binding) {
		return ErrApprovalConsumerBindingInvalid
	}
	_, err := tx.Exec(ctx, `SELECT public.validate_approval_authorization_begin($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		binding.EventID, binding.Durable, binding.Workload, binding.ApprovalRequestID,
		binding.ApprovedRequestVersion, binding.ApprovalEpoch, binding.EventDigest, binding.DigestKind,
		binding.SerializationVersion, binding.Provenance, binding.CommandType, binding.PolicyVersion, binding.ExecutorIdentity)
	if err != nil {
		return approvalConsumerError(ctx, err)
	}
	return nil
}

func validateLegacyApprovalAuthorizationBegin(ctx context.Context, tx pgx.Tx, requestID uuid.UUID, executor string) error {
	if requestID == uuid.Nil || (executor != "billing-command-executor" && executor != "token-command-executor") {
		return ErrApprovalConsumerBindingInvalid
	}
	if _, err := tx.Exec(ctx, `SELECT public.validate_legacy_approval_authorization_begin($1,$2)`, requestID, executor); err != nil {
		return approvalConsumerError(ctx, err)
	}
	return nil
}

func scanApprovalDeliveryState(ctx context.Context, row pgx.Row) (ApprovalDeliveryState, error) {
	var state ApprovalDeliveryState
	if err := row.Scan(&state.DomainAttemptCount, &state.State, &state.Version); err != nil {
		return ApprovalDeliveryState{}, approvalConsumerError(ctx, err)
	}
	return state, nil
}

func validApprovalAuthorizationLookup(eventID uuid.UUID, durable, workload string) bool {
	return eventID != uuid.Nil && boundedApprovalConsumerString(durable, 120) && (workload == "billing-command-executor" || workload == "token-command-executor")
}

func validApprovalAuthorizationBinding(binding ApprovalAuthorizationBinding) bool {
	if !validApprovalAuthorizationLookup(binding.EventID, binding.Durable, binding.Workload) || binding.ApprovalRequestID == uuid.Nil ||
		binding.ApprovedRequestVersion <= 0 || binding.ApprovalEpoch <= 0 || binding.PolicyVersion <= 0 ||
		!lowerHexDigest(binding.EventDigest) || binding.ExecutorIdentity != binding.Workload || !boundedApprovalConsumerString(binding.CommandType, 120) {
		return false
	}
	if binding.SerializationVersion != "LEGACY_TYPED_V1" && binding.SerializationVersion != "APPROVAL_RFC8785_DECIMAL_V1" {
		return false
	}
	if binding.DigestKind == "DELIVERY_BYTES" {
		return binding.Provenance == "FROZEN_AT_APPROVAL" || binding.Provenance == "RECONSTRUCTED_FOR_DELIVERY"
	}
	return binding.DigestKind == "LEGACY_SEMANTIC" && binding.SerializationVersion == "LEGACY_TYPED_V1" && binding.Provenance == "RECONSTRUCTED_AFTER_PUBLISH"
}

func validApprovalAuthorizationRoute(route ApprovalAuthorizationRoute) bool {
	binding := ApprovalAuthorizationBinding{EventID: route.EventID, Durable: route.CanonicalDurable, Workload: route.CanonicalWorkload,
		ApprovalRequestID: route.ApprovalRequestID, ApprovedRequestVersion: route.ApprovedRequestVersion, ApprovalEpoch: route.ApprovalEpoch,
		SerializationVersion: route.SerializationVersion, Provenance: route.Provenance, EventDigest: route.EventDigest,
		DigestKind: route.DigestKind, CommandType: route.CommandType, PolicyVersion: route.PolicyVersion, ExecutorIdentity: route.ExecutorIdentity}
	if !validApprovalAuthorizationBinding(binding) {
		return false
	}
	descriptor, ok := approvalconsumer.LookupDurable(route.CanonicalDurable)
	return ok && descriptor.Workload == route.CanonicalWorkload && approvalconsumer.MatchRoute(descriptor, route.CommandType, route.PolicyVersion, route.ExecutorIdentity) &&
		(route.SerializationVersion == "LEGACY_TYPED_V1" && (route.CanonicalDurable == "billing-command-executor-v1") ||
			route.SerializationVersion == "APPROVAL_RFC8785_DECIMAL_V1" && (route.CanonicalDurable != "billing-command-executor-v1"))
}

func boundedApprovalConsumerString(value string, maximum int) bool {
	if value == "" || len(value) > maximum || value != strings.TrimSpace(value) {
		return false
	}
	for _, current := range value {
		if current < 0x20 || current == 0x7f {
			return false
		}
	}
	return true
}

func lowerHexDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, current := range value {
		if (current < '0' || current > '9') && (current < 'a' || current > 'f') {
			return false
		}
	}
	return true
}

func approvalConsumerError(ctx context.Context, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		switch postgresError.Code {
		case "40001":
			// Keep the existing fail-closed HTTP classification while exposing
			// the serialization cause to the caller; do not retry it here.
			return errors.Join(ErrApprovalConsumerBindingUnavailable, err)
		case "42501":
			return ErrForbidden
		case "22023", "23514", "55000", "P0002", "P0003":
			return ErrApprovalConsumerBindingInvalid
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrApprovalConsumerBindingInvalid
	}
	return ErrApprovalConsumerBindingUnavailable
}
