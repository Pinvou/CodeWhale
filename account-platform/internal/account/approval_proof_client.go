package account

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

var (
	ErrApprovalProofRejected    = errors.New("approval proof rejected")
	ErrApprovalProofInvalid     = errors.New("approval proof invalid")
	ErrApprovalProofTooLarge    = errors.New("approval proof too large")
	ErrApprovalProofUnavailable = errors.New("approval proof unavailable")
	ErrApprovalProofTimeout     = errors.New("approval proof timed out")
)

type ApprovalProofResult struct {
	Response approvalencoding.ProofResponse
	Digest   string
}

type ApprovalProofSource interface {
	Proof(context.Context, uuid.UUID, approvalencoding.ProofArtifacts) (ApprovalProofResult, error)
}

type HTTPApprovalProofSource struct {
	billingURL   *url.URL
	tokenURL     *url.URL
	serviceToken string
	client       *http.Client
}

func NewHTTPApprovalProofSource(billingURL, serviceToken string) (*HTTPApprovalProofSource, error) {
	parse := func(raw string) (*url.URL, error) {
		value, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
		if err != nil || value == nil || (value.Scheme != "http" && value.Scheme != "https") || value.Hostname() == "" || value.User != nil || value.RawQuery != "" || value.ForceQuery || value.Fragment != "" || value.Path != "" {
			return nil, ErrApprovalProofUnavailable
		}
		return value, nil
	}
	billing, err := parse(billingURL)
	if err != nil || strings.TrimSpace(serviceToken) == "" {
		return nil, ErrApprovalProofUnavailable
	}
	client := observability.NewHTTPClient(2 * time.Second)
	// This is a credentialed internal endpoint, not a navigable URL. Even a
	// same-host redirect must not select a different proof authority/path.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &HTTPApprovalProofSource{billingURL: billing, serviceToken: strings.TrimSpace(serviceToken), client: client}, nil
}

func (source *HTTPApprovalProofSource) Proof(ctx context.Context, requestID uuid.UUID, artifacts approvalencoding.ProofArtifacts) (ApprovalProofResult, error) {
	if source == nil || source.client == nil {
		return ApprovalProofResult{}, ErrApprovalProofUnavailable
	}
	payload, err := approvalencoding.EncodeProofRequest(requestID, artifacts)
	if err != nil {
		return ApprovalProofResult{}, ErrApprovalProofInvalid
	}
	base := source.billingURL
	switch artifacts.CommandType {
	case "WALLET_ADJUSTMENT", "PAYMENT_PROVIDER_RESUME", "PAYMENT_PRODUCT_RESUME":
	case "TOKEN_MODEL_LIFECYCLE_CHANGE", "TOKEN_MODEL_CATALOG_ROLLBACK", "TOKEN_CREDENTIAL_REVOKE":
		base = source.tokenURL
	default:
		return ApprovalProofResult{}, ErrApprovalProofInvalid
	}

	if base == nil {
		return ApprovalProofResult{}, ErrApprovalProofUnavailable
	}

	// This absolute deadline is created once, after local encoding and before
	// any network operation. It includes the entire response read/validation.
	deadline := time.Now().Add(2 * time.Second)
	if prior, ok := ctx.Deadline(); ok && prior.Before(deadline) {
		deadline = prior
	}
	requestCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, base.String()+"/internal/v1/approval-command-proofs", bytes.NewReader(payload))
	if err != nil {
		return ApprovalProofResult{}, ErrApprovalProofUnavailable
	}
	if err := serviceauth.SetAuthorization(request, source.serviceToken); err != nil {
		return ApprovalProofResult{}, ErrApprovalProofUnavailable
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := source.client.Do(request)
	if err != nil {
		return ApprovalProofResult{}, approvalProofNetworkError(err, requestCtx)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 2048))
		if response.StatusCode == http.StatusGatewayTimeout || errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
			return ApprovalProofResult{}, ErrApprovalProofTimeout
		}
		return ApprovalProofResult{}, ErrApprovalProofUnavailable
	}
	maximumResponse := approvalencoding.MaximumDocumentBytes - len(payload)
	if maximumResponse <= 0 {
		return ApprovalProofResult{}, ErrApprovalProofTooLarge
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, int64(maximumResponse)+1))
	if err != nil {
		return ApprovalProofResult{}, approvalProofNetworkError(err, requestCtx)
	}
	if len(raw) > maximumResponse || !approvalencoding.ProofTransportWithinBudget(payload, raw) {
		return ApprovalProofResult{}, ErrApprovalProofTooLarge
	}
	if err := requestCtx.Err(); err != nil {
		return ApprovalProofResult{}, approvalProofNetworkError(err, requestCtx)
	}
	proof, digest, err := approvalencoding.DecodeProofResponse(raw)
	if err != nil || !approvalencoding.ProofResponseBound(proof, requestID, artifacts) || !approvalencoding.ProofResponseFresh(proof, time.Now().UTC()) {
		return ApprovalProofResult{}, ErrApprovalProofInvalid
	}
	if !approvalProofReasonAllowed(proof.CommandType, proof.Verdict, proof.ReasonCode) {
		return ApprovalProofResult{}, ErrApprovalProofInvalid
	}
	if err := requestCtx.Err(); err != nil {
		return ApprovalProofResult{}, approvalProofNetworkError(err, requestCtx)
	}
	result := ApprovalProofResult{Response: proof, Digest: digest}
	if proof.Verdict == "DENY" {
		// Preserve only the validated closed response for stable redacted audit.
		return result, ErrApprovalProofRejected
	}
	return result, nil
}

func approvalProofReasonAllowed(command, verdict, reason string) bool {
	switch command {
	case "WALLET_ADJUSTMENT", "PAYMENT_PROVIDER_RESUME", "PAYMENT_PRODUCT_RESUME", "TOKEN_MODEL_LIFECYCLE_CHANGE", "TOKEN_MODEL_CATALOG_ROLLBACK", "TOKEN_CREDENTIAL_REVOKE":
	default:
		return false
	}

	if verdict == "ALLOW" {
		return reason == "PROOF_VALID"
	}
	if verdict != "DENY" {
		return false
	}
	common := map[string]bool{
		"RESOURCE_NOT_FOUND": true, "RESOURCE_STATE_CHANGED": true, "RESOURCE_VERSION_CHANGED": true,
		"SNAPSHOT_MISMATCH": true, "POLICY_VERSION_CHANGED": true, "DOMAIN_INVARIANT_FAILED": true,
		"EXECUTION_WINDOW_EXPIRED": true,
	}
	if common[reason] {
		return true
	}
	switch command {
	case "WALLET_ADJUSTMENT":
		return false
	case "PAYMENT_PROVIDER_RESUME", "PAYMENT_PRODUCT_RESUME":
		return reason == "ACTIVATION_CHANGED" || reason == "EVIDENCE_MISSING" || reason == "EVIDENCE_EXPIRED" || reason == "PROVIDER_STATE_CHANGED"
	case "TOKEN_MODEL_LIFECYCLE_CHANGE", "TOKEN_MODEL_CATALOG_ROLLBACK":
		return reason == "ACTIVATION_CHANGED" || reason == "EVIDENCE_MISSING" || reason == "EVIDENCE_EXPIRED"
	case "TOKEN_CREDENTIAL_REVOKE":
		return reason == "ACTIVATION_CHANGED"
	default:
		return false
	}
}

func approvalProofNetworkError(err error, ctx context.Context) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ErrApprovalProofTimeout
	}
	return ErrApprovalProofUnavailable
}

func NewHTTPApprovalProofSourceWithToken(billingURL, tokenURL, credential string) (*HTTPApprovalProofSource, error) {
	source, err := NewHTTPApprovalProofSource(billingURL, credential)
	if err != nil {
		return nil, err
	}
	target, err := NewHTTPApprovalProofSource(tokenURL, credential)
	if err != nil {
		return nil, err
	}
	source.tokenURL = target.billingURL
	return source, nil
}
