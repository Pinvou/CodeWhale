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

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

var ErrApprovalRecoveryConfirmationUnavailable = errors.New("approval recovery confirmation unavailable")

// ApprovalRecoveryConfirmationClient carries only Account's original
// candidate and live query lease. It never carries domain-result evidence.
type ApprovalRecoveryConfirmationClient struct {
	endpoint     *url.URL
	serviceToken string
	httpClient   *http.Client
}

func NewApprovalRecoveryConfirmationClient(baseURL, serviceToken string) (*ApprovalRecoveryConfirmationClient, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") || strings.TrimSpace(serviceToken) == "" {
		return nil, ErrApprovalRecoveryConfirmationUnavailable
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/internal/v1/approval-recovery/confirm"
	parsed.RawPath = ""
	client := observability.NewHTTPClient(5 * time.Second)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &ApprovalRecoveryConfirmationClient{endpoint: parsed, serviceToken: serviceToken, httpClient: client}, nil
}

func (c *ApprovalRecoveryConfirmationClient) ConfirmOriginalSuccess(ctx context.Context, candidate ApprovalRecoveryCandidate, lease RecoveryQueryLease) error {
	if c == nil || c.endpoint == nil || c.httpClient == nil || !validRecoveryInspectionCandidate(candidate) || candidate.OriginalKey == "" ||
		lease.RequestID != candidate.RequestID || lease.Token == uuid.Nil || !lease.Until.After(time.Now().UTC()) {
		return ErrApprovalRecoveryConfirmationUnavailable
	}
	body, err := json.Marshal(approvalRecoveryConfirmation{Candidate: candidate, Lease: lease})
	if err != nil || len(body) > 16<<10 {
		return ErrApprovalRecoveryConfirmationUnavailable
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(bounded, http.MethodPost, c.endpoint.String(), bytes.NewReader(body))
	if err != nil || serviceauth.SetAuthorization(request, c.serviceToken) != nil {
		return ErrApprovalRecoveryConfirmationUnavailable
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Cache-Control", "no-cache")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return ErrApprovalRecoveryConfirmationUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent || len(response.Header.Values("Cache-Control")) != 1 || response.Header.Get("Cache-Control") != "no-store" {
		return ErrApprovalRecoveryConfirmationUnavailable
	}
	var single [1]byte
	if count, err := response.Body.Read(single[:]); count != 0 || err != io.EOF {
		return ErrApprovalRecoveryConfirmationUnavailable
	}
	return nil
}
