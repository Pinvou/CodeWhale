package account

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

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/google/uuid"
)

type OrganizationLifecycleBlocker struct {
	Code         string `json:"code"`
	Domain       string `json:"domain,omitempty"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
}

type OrganizationLifecycleAssessment struct {
	OrganizationID uuid.UUID                      `json:"organization_id"`
	Status         string                         `json:"status"`
	AccountVersion int64                          `json:"account_version"`
	Ready          bool                           `json:"ready"`
	Blockers       []OrganizationLifecycleBlocker `json:"blockers"`
}

type OrganizationLifecycleClient interface {
	Apply(context.Context, uuid.UUID, string, int64) (OrganizationLifecycleAssessment, error)
	Preflight(context.Context, uuid.UUID, string, int64) (OrganizationLifecycleAssessment, error)
}

type HTTPOrganizationLifecycleClient struct {
	baseURL      *url.URL
	serviceToken string
	domain       string
	client       *http.Client
}

func NewHTTPOrganizationLifecycleClient(rawURL, serviceToken, domain string) (*HTTPOrganizationLifecycleClient, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("BILLING_HTTP_URL must be an absolute URL")
	}
	if strings.TrimSpace(serviceToken) == "" {
		return nil, errors.New("INTERNAL_SERVICE_TOKEN is required")
	}
	domain = strings.ToUpper(strings.TrimSpace(domain))
	if domain != "BILLING" && domain != "TOKEN" {
		return nil, errors.New("organization lifecycle domain must be BILLING or TOKEN")
	}
	client := observability.NewHTTPClient(5 * time.Second)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &HTTPOrganizationLifecycleClient{baseURL: parsed, serviceToken: strings.TrimSpace(serviceToken), domain: domain, client: client}, nil
}

const organizationLifecycleResponseLimit = 1 << 20

func readOrganizationLifecycleResponse(response *http.Response) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(response.Body, organizationLifecycleResponseLimit+1))
	if err != nil || len(body) > organizationLifecycleResponseLimit {
		return nil, errors.New("organization lifecycle response is unavailable")
	}
	return body, nil
}

func (c *HTTPOrganizationLifecycleClient) Apply(ctx context.Context, organizationID uuid.UUID, status string, accountVersion int64) (OrganizationLifecycleAssessment, error) {
	return c.request(ctx, organizationID, status, accountVersion, false)
}

func (c *HTTPOrganizationLifecycleClient) Preflight(ctx context.Context, organizationID uuid.UUID, status string, accountVersion int64) (OrganizationLifecycleAssessment, error) {
	return c.request(ctx, organizationID, status, accountVersion, true)
}

func (c *HTTPOrganizationLifecycleClient) request(ctx context.Context, organizationID uuid.UUID, status string, accountVersion int64, preflightOnly bool) (OrganizationLifecycleAssessment, error) {
	endpoint := *c.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/internal/v1/organizations/" + organizationID.String() + "/lifecycle"
	body, err := json.Marshal(map[string]any{"status": status, "account_version": accountVersion, "preflight_only": preflightOnly})
	if err != nil {
		return OrganizationLifecycleAssessment{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return OrganizationLifecycleAssessment{}, err
	}
	request.Header.Set("Authorization", "Bearer "+c.serviceToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return OrganizationLifecycleAssessment{}, errors.New("organization lifecycle request is unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusConflict {
		return OrganizationLifecycleAssessment{}, fmt.Errorf("%s organization lifecycle returned HTTP %d", c.domain, response.StatusCode)
	}
	responseBody, err := readOrganizationLifecycleResponse(response)
	if err != nil {
		return OrganizationLifecycleAssessment{}, err
	}
	var assessment OrganizationLifecycleAssessment
	if err := json.Unmarshal(responseBody, &assessment); err != nil {
		return OrganizationLifecycleAssessment{}, errors.New("organization lifecycle response is invalid")
	}
	var readiness struct {
		Ready *bool `json:"ready"`
	}
	if json.Unmarshal(responseBody, &readiness) != nil || readiness.Ready == nil || (assessment.Ready && (len(assessment.Blockers) != 0 || response.StatusCode != http.StatusOK)) {
		return OrganizationLifecycleAssessment{}, errors.New("organization lifecycle readiness is invalid")
	}
	for index := range assessment.Blockers {
		assessment.Blockers[index].Domain = c.domain
	}
	if assessment.OrganizationID == uuid.Nil || assessment.OrganizationID != organizationID || assessment.Status != status || assessment.AccountVersion != accountVersion {
		return OrganizationLifecycleAssessment{}, fmt.Errorf("%s organization lifecycle returned an invalid or conflicting projection", c.domain)
	}
	return assessment, nil
}

func (s *Service) applyOrganizationLifecycleProjection(ctx context.Context, organizationID uuid.UUID, status string, accountVersion int64) (OrganizationLifecycleAssessment, error) {
	combined := OrganizationLifecycleAssessment{OrganizationID: organizationID, Status: status, AccountVersion: accountVersion, Ready: true, Blockers: []OrganizationLifecycleBlocker{}}
	for _, client := range s.organizationLifecycle {
		assessment, err := client.Apply(ctx, organizationID, status, accountVersion)
		if err != nil {
			return OrganizationLifecycleAssessment{}, err
		}
		if !assessment.Ready {
			combined.Ready = false
			combined.Blockers = append(combined.Blockers, assessment.Blockers...)
			return combined, nil
		}
	}
	return combined, nil
}

func (s *Service) preflightOrganizationLifecycleProjection(ctx context.Context, organizationID uuid.UUID, status string, accountVersion int64) (OrganizationLifecycleAssessment, error) {
	combined := OrganizationLifecycleAssessment{OrganizationID: organizationID, Status: status, AccountVersion: accountVersion, Ready: true, Blockers: []OrganizationLifecycleBlocker{}}
	for _, client := range s.organizationLifecycle {
		assessment, err := client.Preflight(ctx, organizationID, status, accountVersion)
		if err != nil {
			return OrganizationLifecycleAssessment{}, err
		}
		if !assessment.Ready {
			combined.Ready = false
			combined.Blockers = append(combined.Blockers, assessment.Blockers...)
		}
	}
	return combined, nil
}
