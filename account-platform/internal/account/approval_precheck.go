package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

// The Account proxy is the only surface the business page may use to obtain the
// closed owner projection of a financial repair command before any approval
// draft exists. It forwards a closed request to the Billing precheck endpoint
// with the Account service identity, revalidates the closed answer against the
// command's own target contract and never caches or stores anything: the owner
// domain stays the single source of every frozen fact.

var (
	ErrApprovalPrecheckUnavailable = errors.New("approval precheck unavailable")
	ErrApprovalPrecheckRejected    = errors.New("approval precheck rejected")
	ErrApprovalPrecheckInvalid     = errors.New("approval precheck invalid")
	ErrApprovalPrecheckTooLarge    = errors.New("approval precheck too large")
	ErrApprovalPrecheckTimeout     = errors.New("approval precheck timed out")
)

// approvalPrecheckTargetResourceTypes is the closed command-to-target contract.
// The proxy refuses any answer whose target type is not the one the command
// owns, so a compromised or mismatched owner response can never be presented as
// a different resource.
var approvalPrecheckTargetResourceTypes = map[string]string{
	"ENTITLEMENT_COMPONENT_ADJUSTMENT": "ENTITLEMENT_PACKAGE",
}

// ApprovalPrecheckRequest is the closed precheck input. It mirrors the owner
// wire exactly; empty optional references are dropped instead of being sent as
// zero values, so the forwarded body carries only the facts the page supplied.
type ApprovalPrecheckRequest struct {
	CommandType      string `json:"command_type"`
	TargetResourceID string `json:"target_resource_id"`
	PaymentOrderID   string `json:"payment_order_id"`
	ReasonCode       string `json:"reason_code"`
}

// ApprovalPrecheckProjection is the closed owner answer: exactly the documents
// the Portal needs to render the business page, never Provider text, prompts or
// personal data.
type ApprovalPrecheckProjection struct {
	CommandType              string
	PolicyVersion            int64
	SnapshotSchemaVersion    int
	TargetResourceType       string
	TargetResourceID         string
	NormalizedCommand        json.RawMessage
	CriticalResourceVersions json.RawMessage
	FinancialSnapshot        json.RawMessage
}

type ApprovalPrecheckSource interface {
	Precheck(context.Context, ApprovalPrecheckRequest) (ApprovalPrecheckProjection, error)
}

type HTTPApprovalPrecheckSource struct {
	billingURL   *url.URL
	serviceToken string
	client       *http.Client
}

func NewHTTPApprovalPrecheckSource(billingBaseURL, serviceToken string) (*HTTPApprovalPrecheckSource, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(billingBaseURL), "/"))
	if err != nil || parsed == nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Path != "" || strings.TrimSpace(serviceToken) == "" {
		return nil, ErrApprovalPrecheckUnavailable
	}
	client := observability.NewHTTPClient(2 * time.Second)
	// A credentialed internal endpoint is not a navigable URL: even a same-host
	// redirect must not select a different precheck authority or path.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &HTTPApprovalPrecheckSource{billingURL: parsed, serviceToken: strings.TrimSpace(serviceToken), client: client}, nil
}

// precheckWireResponse mirrors the owner response schema so the proxy can prove
// it received exactly the closed shape before presenting anything to a browser.
type precheckWireResponse struct {
	CommandType              string          `json:"command_type"`
	PolicyVersion            int64           `json:"policy_version"`
	SnapshotSchemaVersion    int             `json:"snapshot_schema_version"`
	TargetResourceType       string          `json:"target_resource_type"`
	TargetResourceID         string          `json:"target_resource_id"`
	NormalizedCommand        json.RawMessage `json:"normalized_command"`
	CriticalResourceVersions json.RawMessage `json:"critical_resource_versions"`
	FinancialSnapshot        json.RawMessage `json:"financial_snapshot"`
}

func (source *HTTPApprovalPrecheckSource) Precheck(ctx context.Context, request ApprovalPrecheckRequest) (ApprovalPrecheckProjection, error) {
	if source == nil || source.client == nil {
		return ApprovalPrecheckProjection{}, ErrApprovalPrecheckUnavailable
	}
	if !validApprovalPrecheckRequest(request) {
		return ApprovalPrecheckProjection{}, ErrApprovalPrecheckInvalid
	}
	payload, err := json.Marshal(request)
	if err != nil || len(payload) > approvalencoding.MaximumDocumentBytes {
		return ApprovalPrecheckProjection{}, ErrApprovalPrecheckInvalid
	}
	// This absolute deadline is created once, after local encoding and before any
	// network operation; it includes the entire response read and validation.
	deadline := time.Now().Add(2 * time.Second)
	if prior, ok := ctx.Deadline(); ok && prior.Before(deadline) {
		deadline = prior
	}
	requestCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(requestCtx, http.MethodPost,
		source.billingURL.String()+"/internal/v1/approval-command-prechecks", bytes.NewReader(payload))
	if err != nil {
		return ApprovalPrecheckProjection{}, ErrApprovalPrecheckUnavailable
	}
	if err := serviceauth.SetAuthorization(httpRequest, source.serviceToken); err != nil {
		return ApprovalPrecheckProjection{}, ErrApprovalPrecheckUnavailable
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Cache-Control", "no-store")
	response, err := source.client.Do(httpRequest)
	if err != nil {
		return ApprovalPrecheckProjection{}, approvalPrecheckNetworkError(err, requestCtx)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusGatewayTimeout || errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 2048))
		return ApprovalPrecheckProjection{}, ErrApprovalPrecheckTimeout
	}
	if response.StatusCode == http.StatusUnprocessableEntity {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 2048))
		return ApprovalPrecheckProjection{}, ErrApprovalPrecheckRejected
	}
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 2048))
		return ApprovalPrecheckProjection{}, ErrApprovalPrecheckUnavailable
	}
	maximumResponse := approvalencoding.MaximumDocumentBytes - len(payload)
	if maximumResponse <= 0 {
		return ApprovalPrecheckProjection{}, ErrApprovalPrecheckTooLarge
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, int64(maximumResponse)+1))
	if err != nil {
		return ApprovalPrecheckProjection{}, approvalPrecheckNetworkError(err, requestCtx)
	}
	if len(raw) > maximumResponse {
		return ApprovalPrecheckProjection{}, ErrApprovalPrecheckTooLarge
	}
	if !utf8.Valid(raw) || !uniqueRecoveryJSONKeys(raw) {
		return ApprovalPrecheckProjection{}, ErrApprovalPrecheckInvalid
	}
	if err := requestCtx.Err(); err != nil {
		return ApprovalPrecheckProjection{}, approvalPrecheckNetworkError(err, requestCtx)
	}
	projection, err := decodeApprovalPrecheckResponse(raw, request)
	if err != nil {
		return ApprovalPrecheckProjection{}, err
	}
	if err := requestCtx.Err(); err != nil {
		return ApprovalPrecheckProjection{}, approvalPrecheckNetworkError(err, requestCtx)
	}
	return projection, nil
}

func decodeApprovalPrecheckResponse(raw []byte, request ApprovalPrecheckRequest) (ApprovalPrecheckProjection, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var wire precheckWireResponse
	if decoder.Decode(&wire) != nil || decoder.Decode(new(any)) != io.EOF {
		return ApprovalPrecheckProjection{}, ErrApprovalPrecheckInvalid
	}
	expectedTargetType, ok := approvalPrecheckTargetResourceTypes[request.CommandType]
	if !ok || wire.CommandType != request.CommandType || wire.TargetResourceType != expectedTargetType ||
		wire.TargetResourceID != request.TargetResourceID || wire.PolicyVersion != 2 || wire.SnapshotSchemaVersion != 2 {
		return ApprovalPrecheckProjection{}, ErrApprovalPrecheckInvalid
	}
	if _, valid := closedApprovalPrecheckDocument(wire.NormalizedCommand); !valid {
		return ApprovalPrecheckProjection{}, ErrApprovalPrecheckInvalid
	}
	if _, valid := closedApprovalPrecheckDocument(wire.CriticalResourceVersions); !valid {
		return ApprovalPrecheckProjection{}, ErrApprovalPrecheckInvalid
	}
	if _, valid := closedApprovalPrecheckDocument(wire.FinancialSnapshot); !valid {
		return ApprovalPrecheckProjection{}, ErrApprovalPrecheckInvalid
	}
	// Validate the same closed v2/s2 documents used by approval proof creation.
	// Transport-level objects alone are not sufficient owner evidence.
	input := approvalencoding.LegacyProofInput{
		CommandType: wire.CommandType, PolicyVersion: wire.PolicyVersion,
		SnapshotSchemaVersion: wire.SnapshotSchemaVersion,
		TargetResourceType:    wire.TargetResourceType, TargetResourceID: wire.TargetResourceID,
		NormalizedCommand: wire.NormalizedCommand, CriticalResourceVersions: wire.CriticalResourceVersions,
		FinancialSnapshot: wire.FinancialSnapshot,
	}
	if _, err := approvalencoding.ConvertLegacyProofInput(input); err != nil {
		return ApprovalPrecheckProjection{}, ErrApprovalPrecheckInvalid
	}
	var selected struct {
		PackageID      string `json:"package_id"`
		PaymentOrderID string `json:"payment_order_id"`
		ReasonCode     string `json:"reason_code"`
	}
	if json.Unmarshal(wire.NormalizedCommand, &selected) != nil ||
		selected.PackageID != request.TargetResourceID || selected.PaymentOrderID != request.PaymentOrderID || selected.ReasonCode != request.ReasonCode {
		return ApprovalPrecheckProjection{}, ErrApprovalPrecheckInvalid
	}
	return ApprovalPrecheckProjection{
		CommandType: wire.CommandType, PolicyVersion: wire.PolicyVersion, SnapshotSchemaVersion: wire.SnapshotSchemaVersion,
		TargetResourceType: wire.TargetResourceType, TargetResourceID: wire.TargetResourceID,
		NormalizedCommand: wire.NormalizedCommand, CriticalResourceVersions: wire.CriticalResourceVersions,
		FinancialSnapshot: wire.FinancialSnapshot,
	}, nil
}

// closedApprovalPrecheckDocument accepts only a non-empty JSON object whose keys
// are unique and whose size is inside the closed document bound.
func closedApprovalPrecheckDocument(raw json.RawMessage) (json.RawMessage, bool) {
	if len(raw) == 0 || len(raw) > approvalencoding.MaximumDocumentBytes || !utf8.Valid(raw) || !uniqueRecoveryJSONKeys(raw) {
		return nil, false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, false
	}
	return raw, true
}

func validApprovalPrecheckRequest(request ApprovalPrecheckRequest) bool {
	if request.CommandType != "ENTITLEMENT_COMPONENT_ADJUSTMENT" {
		return false
	}
	if _, ok := validPrecheckIdentifier(request.TargetResourceID); !ok {
		return false
	}
	if _, ok := validPrecheckIdentifier(request.PaymentOrderID); !ok {
		return false
	}
	return request.ReasonCode != "" && strings.TrimSpace(request.ReasonCode) == request.ReasonCode &&
		len(request.ReasonCode) <= 80
}

func validPrecheckIdentifier(value string) (uuid.UUID, bool) {
	if strings.TrimSpace(value) != value {
		return uuid.Nil, false
	}
	parsed, err := uuid.Parse(value)
	if err != nil || parsed == uuid.Nil || parsed.String() != value {
		return uuid.Nil, false
	}
	return parsed, true
}

func approvalPrecheckNetworkError(err error, ctx context.Context) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ErrApprovalPrecheckTimeout
	}
	return ErrApprovalPrecheckUnavailable
}

// adminApprovalCommandPrecheck is the Account-side proxy. It performs no
// domain read of its own, writes nothing and never caches the answer.
func (s *Service) adminApprovalCommandPrecheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.approvalPrechecks == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_PRECHECK_UNAVAILABLE",
			"the read-only precheck authority is not configured", httpserver.RequestID(r.Context()))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	stopClose := context.AfterFunc(ctx, func() { _ = r.Body.Close() })
	defer stopClose()
	body, err := io.ReadAll(io.LimitReader(r.Body, approvalencoding.MaximumDocumentBytes+1))
	if ctx.Err() != nil {
		api.WriteError(w, http.StatusGatewayTimeout, "APPROVAL_PRECHECK_TIMEOUT", "the read-only precheck deadline expired", httpserver.RequestID(r.Context()))
		return
	}
	if err != nil || len(body) > approvalencoding.MaximumDocumentBytes {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_PRECHECK_REQUEST",
			"precheck request exceeds the closed transport bound", httpserver.RequestID(r.Context()))
		return
	}
	if !utf8.Valid(body) || !uniqueRecoveryJSONKeys(body) {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_PRECHECK_REQUEST",
			"precheck request is invalid", httpserver.RequestID(r.Context()))
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var request ApprovalPrecheckRequest
	if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF || !validApprovalPrecheckRequest(request) {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_PRECHECK_REQUEST",
			"precheck request is invalid", httpserver.RequestID(r.Context()))
		return
	}
	projection, err := s.approvalPrechecks.Precheck(ctx, request)
	switch {
	case errors.Is(err, ErrApprovalPrecheckTimeout):
		api.WriteError(w, http.StatusGatewayTimeout, "APPROVAL_PRECHECK_TIMEOUT",
			"the read-only precheck deadline expired", httpserver.RequestID(r.Context()))
		return
	case errors.Is(err, ErrApprovalPrecheckRejected):
		api.WriteError(w, http.StatusUnprocessableEntity, "APPROVAL_PRECHECK_UNAVAILABLE",
			"the closed owner projection is not available for this target", httpserver.RequestID(r.Context()))
		return
	case err != nil:
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_PRECHECK_UNAVAILABLE",
			"the read-only precheck is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	encoded, err := json.Marshal(approvalPrecheckAdminResponse{
		CommandType: projection.CommandType, PolicyVersion: projection.PolicyVersion,
		SnapshotSchemaVersion: projection.SnapshotSchemaVersion,
		TargetResourceType:    projection.TargetResourceType, TargetResourceID: projection.TargetResourceID,
		NormalizedCommand: projection.NormalizedCommand, CriticalResourceVersions: projection.CriticalResourceVersions,
		FinancialSnapshot: projection.FinancialSnapshot,
	})
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_PRECHECK_UNAVAILABLE",
			"the read-only precheck is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

type approvalPrecheckAdminResponse struct {
	CommandType              string          `json:"command_type"`
	PolicyVersion            int64           `json:"policy_version"`
	SnapshotSchemaVersion    int             `json:"snapshot_schema_version"`
	TargetResourceType       string          `json:"target_resource_type"`
	TargetResourceID         string          `json:"target_resource_id"`
	NormalizedCommand        json.RawMessage `json:"normalized_command"`
	CriticalResourceVersions json.RawMessage `json:"critical_resource_versions"`
	FinancialSnapshot        json.RawMessage `json:"financial_snapshot"`
}
