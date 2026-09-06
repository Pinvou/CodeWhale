package account

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
)

type ApprovalSnapshotRequest struct {
	CommandType        string          `json:"command_type"`
	TargetResourceType string          `json:"target_resource_type"`
	TargetResourceID   string          `json:"target_resource_id"`
	NormalizedCommand  json.RawMessage `json:"normalized_command"`
}

// Only a fresh model-pricing observation may advance its observation clock.
// Stored approval snapshots and idempotency fingerprints remain unchanged.
func sameApprovalFinancialFacts(commandType string, observed, frozen json.RawMessage) bool {
	if commandType == "TOKEN_MODEL_LIFECYCLE_CHANGE" || commandType == "TOKEN_MODEL_CATALOG_ROLLBACK" {
		return approvalpolicy.SameTokenModelFinancialFacts(observed, frozen)
	}
	return sameJSONObject(observed, frozen)
}

type ApprovalSnapshotProjection struct {
	CommandType              string          `json:"command_type"`
	TargetResourceType       string          `json:"target_resource_type"`
	TargetResourceID         string          `json:"target_resource_id"`
	CriticalResourceVersions json.RawMessage `json:"critical_resource_versions"`
	FinancialSnapshot        json.RawMessage `json:"financial_snapshot"`
	PrecheckFingerprint      string          `json:"precheck_fingerprint"`
	tokenObservedAt          time.Time
	billingObservedAt        *time.Time
}

type ApprovalSnapshotSource interface {
	PrecheckApprovalSnapshot(context.Context, ApprovalSnapshotRequest) (ApprovalSnapshotProjection, error)
}

type HTTPApprovalSnapshotSource struct {
	baseURL      *url.URL
	serviceToken string
	client       *http.Client
}

func NewHTTPApprovalSnapshotSource(rawURL, serviceToken string) (*HTTPApprovalSnapshotSource, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || strings.TrimSpace(serviceToken) == "" {
		return nil, errors.New("approval snapshot source configuration is invalid")
	}
	return &HTTPApprovalSnapshotSource{baseURL: parsed, serviceToken: strings.TrimSpace(serviceToken), client: observability.NewHTTPClient(5 * time.Second)}, nil
}

func (c *HTTPApprovalSnapshotSource) PrecheckApprovalSnapshot(ctx context.Context, input ApprovalSnapshotRequest) (ApprovalSnapshotProjection, error) {
	payload, err := json.Marshal(input)
	if err != nil {
		return ApprovalSnapshotProjection{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL.String()+"/internal/v1/approval-command-prechecks", bytes.NewReader(payload))
	if err != nil {
		return ApprovalSnapshotProjection{}, err
	}
	if err := serviceauth.SetAuthorization(request, c.serviceToken); err != nil {
		return ApprovalSnapshotProjection{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ApprovalSnapshotProjection{}, ErrApprovalSnapshotTimeout
		}
		return ApprovalSnapshotProjection{}, ErrApprovalSnapshotUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 2048))
		if response.StatusCode == http.StatusGatewayTimeout {
			return ApprovalSnapshotProjection{}, ErrApprovalSnapshotTimeout
		}
		if response.StatusCode >= 500 {
			return ApprovalSnapshotProjection{}, ErrApprovalSnapshotUnavailable
		}
		return ApprovalSnapshotProjection{}, ErrApprovalSnapshotRejected
	}
	var wire struct {
		CommandType              string          `json:"command_type"`
		TargetResourceType       string          `json:"target_resource_type"`
		TargetResourceID         string          `json:"target_resource_id"`
		CriticalResourceVersions json.RawMessage `json:"critical_resource_versions"`
		FinancialSnapshot        json.RawMessage `json:"financial_snapshot"`
		PrecheckFingerprint      string          `json:"precheck_fingerprint"`
		TokenObservedAt          time.Time       `json:"token_observed_at"`
		BillingObservedAt        *time.Time      `json:"billing_observed_at"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 512<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&wire) != nil || wire.CommandType != input.CommandType || wire.TargetResourceType != input.TargetResourceType || wire.TargetResourceID != input.TargetResourceID || wire.TokenObservedAt.IsZero() || input.CommandType == "TOKEN_CREDENTIAL_REVOKE" && wire.BillingObservedAt != nil || input.CommandType != "TOKEN_CREDENTIAL_REVOKE" && (wire.BillingObservedAt == nil || wire.BillingObservedAt.IsZero()) || len(wire.PrecheckFingerprint) != 64 {
		return ApprovalSnapshotProjection{}, ErrApprovalSnapshotUnavailable
	}
	if decoded, err := hex.DecodeString(wire.PrecheckFingerprint); err != nil || len(decoded) != 32 {
		return ApprovalSnapshotProjection{}, ErrApprovalSnapshotUnavailable
	}
	projection := ApprovalSnapshotProjection{CommandType: wire.CommandType, TargetResourceType: wire.TargetResourceType, TargetResourceID: wire.TargetResourceID, CriticalResourceVersions: wire.CriticalResourceVersions, FinancialSnapshot: wire.FinancialSnapshot, PrecheckFingerprint: wire.PrecheckFingerprint, tokenObservedAt: wire.TokenObservedAt, billingObservedAt: wire.BillingObservedAt}
	policy, err := approvalpolicy.Lookup(wire.CommandType)
	if err != nil {
		return ApprovalSnapshotProjection{}, ErrApprovalSnapshotUnavailable
	}
	critical, err := canonicalApprovalCriticalVersions(policy, projection.CriticalResourceVersions)
	if err != nil {
		return ApprovalSnapshotProjection{}, ErrApprovalSnapshotUnavailable
	}
	financial, err := canonicalApprovalFinancialSnapshot(policy, projection.FinancialSnapshot)
	if err != nil || !tokenApprovalSnapshotsAligned(critical, financial) {
		return ApprovalSnapshotProjection{}, ErrApprovalSnapshotUnavailable
	}
	projection.CriticalResourceVersions, projection.FinancialSnapshot = critical, financial
	return projection, nil
}

func (r *PostgresRepository) WithApprovalSnapshotSource(source ApprovalSnapshotSource) *PostgresRepository {
	r.approvalSnapshots = source
	return r
}

func (r *PostgresRepository) authoritativeApprovalDraft(ctx context.Context, draft ApprovalDraft) (ApprovalDraft, error) {
	if !approvalSnapshotCommand(draft.CommandType) {
		return draft, nil
	}
	if r.approvalSnapshots == nil {
		return ApprovalDraft{}, ErrApprovalSnapshotUnavailable
	}
	projection, err := r.precheckApprovalSnapshot(ctx, ApprovalSnapshotRequest{CommandType: strings.ToUpper(strings.TrimSpace(draft.CommandType)), TargetResourceType: strings.ToUpper(strings.TrimSpace(draft.TargetResourceType)), TargetResourceID: strings.TrimSpace(draft.TargetResourceID), NormalizedCommand: draft.NormalizedCommand})
	if err != nil {
		return ApprovalDraft{}, err
	}
	draft.CriticalResourceVersions = projection.CriticalResourceVersions
	draft.FinancialSnapshot = projection.FinancialSnapshot
	return draft, nil
}

func (r *PostgresRepository) precheckApprovalSnapshot(ctx context.Context, input ApprovalSnapshotRequest) (ApprovalSnapshotProjection, error) {
	if r.approvalSnapshots == nil {
		return ApprovalSnapshotProjection{}, ErrApprovalSnapshotUnavailable
	}
	deadline := time.Now().Add(5 * time.Second)
	if existing, ok := ctx.Deadline(); ok && existing.Before(deadline) {
		deadline = existing
	}
	requestCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	projection, err := r.approvalSnapshots.PrecheckApprovalSnapshot(requestCtx, input)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
		return ApprovalSnapshotProjection{}, ErrApprovalSnapshotTimeout
	}
	return projection, err
}

func approvalSnapshotCommand(commandType string) bool {
	switch strings.ToUpper(strings.TrimSpace(commandType)) {
	case "TOKEN_MODEL_LIFECYCLE_CHANGE", "TOKEN_MODEL_CATALOG_ROLLBACK", "TOKEN_CREDENTIAL_REVOKE":
		return true
	default:
		return false
	}
}
