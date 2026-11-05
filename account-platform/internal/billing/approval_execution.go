package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const billingApprovalExecutorIdentity = "billing-command-executor"

type ApprovedCommand struct {
	ApprovalRequestID             uuid.UUID       `json:"approval_request_id"`
	CommandType                   string          `json:"command_type"`
	PolicyVersion                 int64           `json:"policy_version"`
	CommandFingerprint            string          `json:"command_fingerprint"`
	NormalizedCommand             json.RawMessage `json:"normalized_command"`
	TargetResourceType            string          `json:"target_resource_type"`
	TargetResourceID              string          `json:"target_resource_id"`
	CriticalResourceVersions      json.RawMessage `json:"critical_resource_versions"`
	FinancialSnapshot             json.RawMessage `json:"financial_snapshot"`
	RequesterUserID               uuid.UUID       `json:"requester_user_id"`
	RequesterAuthorizationVersion int64           `json:"requester_authorization_version"`
	RequesterReason               string          `json:"requester_reason"`
	FirstClaim                    bool            `json:"first_claim"`
	ApproverUserID                uuid.UUID       `json:"approver_user_id"`
	DecisionReason                string          `json:"decision_reason"`
	ExecutionIdempotencyKey       string          `json:"execution_idempotency_key"`
	ExecutorIdentity              string          `json:"executor_identity"`
	Status                        string          `json:"status"`
	AuthorizationEventID          *uuid.UUID      `json:"authorization_event_id,omitempty"`
	AuthorizationDurable          string          `json:"authorization_durable,omitempty"`
	AuthorizationApprovedVersion  int64           `json:"authorization_approved_request_version,omitempty"`
	AuthorizationEventDigest      string          `json:"authorization_event_digest,omitempty"`
	AuthorizationDigestKind       string          `json:"authorization_digest_kind,omitempty"`
	AuthorizationSerialization    string          `json:"authorization_serialization_version,omitempty"`
	AuthorizationProvenance       string          `json:"authorization_provenance,omitempty"`
	AuthorizationApprovalEpoch    int64           `json:"authorization_approval_epoch,omitempty"`
}

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

func (route ApprovalAuthorizationRoute) binding() ApprovalAuthorizationBinding {
	return ApprovalAuthorizationBinding{EventID: route.EventID, Durable: route.CanonicalDurable, Workload: route.CanonicalWorkload,
		ApprovalRequestID: route.ApprovalRequestID, ApprovedRequestVersion: route.ApprovedRequestVersion, ApprovalEpoch: route.ApprovalEpoch,
		SerializationVersion: route.SerializationVersion, Provenance: route.Provenance, EventDigest: route.EventDigest,
		DigestKind: route.DigestKind, CommandType: route.CommandType, PolicyVersion: route.PolicyVersion, ExecutorIdentity: route.ExecutorIdentity}
}

type ApprovalAuthorizationDeliveryState struct {
	DomainAttemptCount  int16   `json:"domain_attempt_count"`
	State               string  `json:"state"`
	Version             int64   `json:"version"`
	LastErrorCode       *string `json:"last_error_code,omitempty"`
	ExecutionStatus     *string `json:"execution_status,omitempty"`
	ExecutionErrorClass *string `json:"execution_error_class,omitempty"`
}

type ApprovalExecutionGate interface {
	Begin(context.Context, uuid.UUID, string, json.RawMessage) (ApprovedCommand, error)
	Complete(context.Context, uuid.UUID, string, string, json.RawMessage, json.RawMessage, string) error
}

type HTTPApprovalExecutionGate struct {
	baseURL      *url.URL
	serviceToken string
	httpClient   *http.Client
}

type approvalGateHTTPError struct {
	StatusCode int
	Code       string
}

func (err approvalGateHTTPError) Error() string {
	return fmt.Sprintf("approval execution gate returned HTTP %d (%s)", err.StatusCode, err.Code)
}

func approvalGateUnavailable(err error) bool {
	var responseError approvalGateHTTPError
	return !errors.As(err, &responseError) || responseError.StatusCode >= http.StatusInternalServerError
}

func NewHTTPApprovalExecutionGate(rawURL, serviceToken string) (*HTTPApprovalExecutionGate, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("ACCOUNT_HTTP_URL must be an absolute URL")
	}
	serviceToken = strings.TrimSpace(serviceToken)
	if serviceToken == "" {
		return nil, errors.New("INTERNAL_SERVICE_TOKEN is required")
	}
	return &HTTPApprovalExecutionGate{baseURL: parsed, serviceToken: serviceToken, httpClient: observability.NewHTTPClient(5 * time.Second)}, nil
}

func (gate *HTTPApprovalExecutionGate) Begin(ctx context.Context, requestID uuid.UUID, jobRunID string, observedVersions json.RawMessage) (ApprovedCommand, error) {
	payload := map[string]any{"job_run_id": jobRunID, "observed_versions": json.RawMessage(observedVersions)}
	var command ApprovedCommand
	if err := gate.request(ctx, http.MethodPost, fmt.Sprintf("/internal/v1/approval-requests/%s/executions", requestID), payload, &command); err != nil {
		return ApprovedCommand{}, err
	}
	return command, nil
}

func (gate *HTTPApprovalExecutionGate) ResolveAuthorizationBinding(ctx context.Context, eventID uuid.UUID, durable string) (ApprovalAuthorizationBinding, error) {
	var binding ApprovalAuthorizationBinding
	path := fmt.Sprintf("/internal/v1/approval-authorization/events/%s/binding?durable=%s", eventID, url.QueryEscape(durable))
	if err := gate.request(ctx, http.MethodGet, path, nil, &binding); err != nil {
		return ApprovalAuthorizationBinding{}, err
	}
	return binding, nil
}

func (gate *HTTPApprovalExecutionGate) ResolveAuthorizationRoute(ctx context.Context, eventID uuid.UUID) (ApprovalAuthorizationRoute, error) {
	var route ApprovalAuthorizationRoute
	path := fmt.Sprintf("/internal/v1/approval-authorization/events/%s/route", eventID)
	if err := gate.request(ctx, http.MethodGet, path, nil, &route); err != nil {
		return ApprovalAuthorizationRoute{}, err
	}
	return route, nil
}

func (gate *HTTPApprovalExecutionGate) ObserveAuthorizationDelivery(ctx context.Context, binding ApprovalAuthorizationBinding, stream string, sequence approvalconsumer.Sequence) (ApprovalAuthorizationDeliveryState, error) {
	payload := map[string]any{
		"approval_request_id": binding.ApprovalRequestID, "approved_request_version": binding.ApprovedRequestVersion,
		"approval_epoch": binding.ApprovalEpoch, "serialization_version": binding.SerializationVersion,
		"provenance": binding.Provenance, "event_digest": binding.EventDigest, "digest_kind": binding.DigestKind,
		"command_type": binding.CommandType, "policy_version": binding.PolicyVersion, "executor_identity": binding.ExecutorIdentity,
		"stream": stream, "stream_sequence": sequence,
	}
	return gate.authorizationDeliveryRequest(ctx, binding, "observe", payload)
}

func (gate *HTTPApprovalExecutionGate) AuthorizationDeliveryState(ctx context.Context, binding ApprovalAuthorizationBinding) (ApprovalAuthorizationDeliveryState, error) {
	var state ApprovalAuthorizationDeliveryState
	path := fmt.Sprintf("/internal/v1/approval-authorization/events/%s/deliveries/%s", binding.EventID, url.PathEscape(binding.Durable))
	if err := gate.request(ctx, http.MethodGet, path, nil, &state); err != nil {
		return ApprovalAuthorizationDeliveryState{}, err
	}
	return state, nil
}

func (gate *HTTPApprovalExecutionGate) BeginAuthorized(ctx context.Context, binding ApprovalAuthorizationBinding, jobRunID string, observedVersions, observedFinancialSnapshot json.RawMessage) (ApprovedCommand, error) {
	payload := map[string]any{"job_run_id": jobRunID, "observed_versions": json.RawMessage(observedVersions), "observed_financial_snapshot": json.RawMessage(observedFinancialSnapshot), "authorization_binding": binding}
	var command ApprovedCommand
	if err := gate.request(ctx, http.MethodPost, fmt.Sprintf("/internal/v1/approval-requests/%s/executions", binding.ApprovalRequestID), payload, &command); err != nil {
		return ApprovedCommand{}, err
	}
	return command, nil
}

func (gate *HTTPApprovalExecutionGate) GrantAuthorizationAttempt(ctx context.Context, binding ApprovalAuthorizationBinding, version int64) (ApprovalAuthorizationDeliveryState, error) {
	return gate.authorizationDeliveryRequest(ctx, binding, "attempts/grant", map[string]any{"expected_version": version})
}

func (gate *HTTPApprovalExecutionGate) RecordAuthorizationFailure(ctx context.Context, binding ApprovalAuthorizationBinding, version int64, code string) (ApprovalAuthorizationDeliveryState, error) {
	return gate.authorizationDeliveryRequest(ctx, binding, "attempts/failure", map[string]any{"expected_version": version, "error_code": code})
}

func (gate *HTTPApprovalExecutionGate) RecordAuthorizationResult(ctx context.Context, binding ApprovalAuthorizationBinding, version int64) (ApprovalAuthorizationDeliveryState, error) {
	return gate.authorizationDeliveryRequest(ctx, binding, "result", map[string]any{"expected_version": version})
}

func (gate *HTTPApprovalExecutionGate) RecordAuthorizationDLQ(ctx context.Context, binding ApprovalAuthorizationBinding, version int64, messageID string, sequence approvalconsumer.Sequence) (ApprovalAuthorizationDeliveryState, error) {
	return gate.authorizationDeliveryRequest(ctx, binding, "dlq-puback", map[string]any{"expected_version": version, "message_id": messageID, "stream_sequence": sequence})
}

func (gate *HTTPApprovalExecutionGate) RecordAuthorizationTerminal(ctx context.Context, binding ApprovalAuthorizationBinding, version int64) (ApprovalAuthorizationDeliveryState, error) {
	return gate.authorizationDeliveryRequest(ctx, binding, "terminal", map[string]any{"expected_version": version})
}

func (gate *HTTPApprovalExecutionGate) authorizationDeliveryRequest(ctx context.Context, binding ApprovalAuthorizationBinding, action string, payload any) (ApprovalAuthorizationDeliveryState, error) {
	var state ApprovalAuthorizationDeliveryState
	path := fmt.Sprintf("/internal/v1/approval-authorization/events/%s/deliveries/%s/%s", binding.EventID, url.PathEscape(binding.Durable), action)
	if err := gate.request(ctx, http.MethodPost, path, payload, &state); err != nil {
		return ApprovalAuthorizationDeliveryState{}, err
	}
	return state, nil
}

func (gate *HTTPApprovalExecutionGate) Complete(ctx context.Context, requestID uuid.UUID, jobRunID, result string, domainResult, ledgerReferences json.RawMessage, errorClass string) error {
	payload := map[string]any{"job_run_id": jobRunID, "result": result, "domain_result": json.RawMessage(domainResult), "ledger_references": json.RawMessage(ledgerReferences), "error_class": errorClass}
	return gate.request(ctx, http.MethodPost, fmt.Sprintf("/internal/v1/approval-requests/%s/executions/complete", requestID), payload, nil)
}

func (gate *HTTPApprovalExecutionGate) request(ctx context.Context, method, path string, payload any, output any) error {
	var body []byte
	var err error
	if payload != nil {
		body, err = json.Marshal(payload)
	}
	if err != nil {
		return err
	}
	endpoint := *gate.baseURL
	relative, err := url.Parse(path)
	if err != nil || !strings.HasPrefix(relative.Path, "/") {
		return errors.New("approval execution gate path is invalid")
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + relative.Path
	endpoint.RawQuery = relative.RawQuery
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	if err := serviceauth.SetAuthorization(request, gate.serviceToken); err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := gate.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var body struct {
			Code string `json:"code"`
		}
		_ = json.NewDecoder(response.Body).Decode(&body)
		return approvalGateHTTPError{StatusCode: response.StatusCode, Code: body.Code}
	}
	if output != nil {
		decoder := json.NewDecoder(response.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(output); err != nil {
			return err
		}
		if decoder.Decode(&struct{}{}) != io.EOF {
			return errors.New("approval execution gate returned trailing content")
		}
	}
	return nil
}

type approvalAdjustmentRepository interface {
	AdjustmentForExecution(context.Context, uuid.UUID) (ManualAdjustment, error)
	ExecuteApprovedAdjustment(context.Context, uuid.UUID, uuid.UUID, string, uuid.UUID, string) (ManualAdjustment, error)
}

func (r *PostgresRepository) AdjustmentForExecution(ctx context.Context, adjustmentID uuid.UUID) (ManualAdjustment, error) {
	adjustment, err := getAdjustment(ctx, r.pool, adjustmentID, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return ManualAdjustment{}, ErrNotFound
	}
	return adjustment, err
}
