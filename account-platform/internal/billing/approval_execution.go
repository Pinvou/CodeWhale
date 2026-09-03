package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const billingApprovalExecutorIdentity = "billing-command-executor"

type ApprovedCommand struct {
	ApprovalRequestID        uuid.UUID       `json:"approval_request_id"`
	CommandType              string          `json:"command_type"`
	NormalizedCommand        json.RawMessage `json:"normalized_command"`
	TargetResourceType       string          `json:"target_resource_type"`
	TargetResourceID         string          `json:"target_resource_id"`
	CriticalResourceVersions json.RawMessage `json:"critical_resource_versions"`
	RequesterUserID          uuid.UUID       `json:"requester_user_id"`
	ApproverUserID           uuid.UUID       `json:"approver_user_id"`
	DecisionReason           string          `json:"decision_reason"`
	ExecutionIdempotencyKey  string          `json:"execution_idempotency_key"`
	Status                   string          `json:"status"`
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

func (gate *HTTPApprovalExecutionGate) Complete(ctx context.Context, requestID uuid.UUID, jobRunID, result string, domainResult, ledgerReferences json.RawMessage, errorClass string) error {
	payload := map[string]any{"job_run_id": jobRunID, "result": result, "domain_result": json.RawMessage(domainResult), "ledger_references": json.RawMessage(ledgerReferences), "error_class": errorClass}
	return gate.request(ctx, http.MethodPost, fmt.Sprintf("/internal/v1/approval-requests/%s/executions/complete", requestID), payload, nil)
}

func (gate *HTTPApprovalExecutionGate) request(ctx context.Context, method, path string, payload any, output any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	endpoint := *gate.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + path
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
		return json.NewDecoder(response.Body).Decode(output)
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
