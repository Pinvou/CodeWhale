package account

import (
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

type AccountClosureBlocker struct {
	Code         string `json:"code"`
	Domain       string `json:"domain"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id,omitempty"`
	UserAction   string `json:"user_action,omitempty"`
}

type AccountClosureAssessment struct {
	UserID   uuid.UUID               `json:"user_id"`
	Ready    bool                    `json:"ready"`
	Blockers []AccountClosureBlocker `json:"blockers"`
}

type AccountClosureClient interface {
	Assess(context.Context, uuid.UUID) (AccountClosureAssessment, error)
}

type HTTPAccountClosureClient struct {
	baseURL      *url.URL
	serviceToken string
	domain       string
	client       *http.Client
}

func NewHTTPAccountClosureClient(rawURL, serviceToken, domain string) (*HTTPAccountClosureClient, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("account closure service URL must be absolute")
	}
	domain = strings.ToUpper(strings.TrimSpace(domain))
	if strings.TrimSpace(serviceToken) == "" || (domain != "BILLING" && domain != "TOKEN") {
		return nil, errors.New("account closure client requires a service token and BILLING or TOKEN domain")
	}
	return &HTTPAccountClosureClient{baseURL: parsed, serviceToken: strings.TrimSpace(serviceToken), domain: domain, client: observability.NewHTTPClient(5 * time.Second)}, nil
}

func (c *HTTPAccountClosureClient) Assess(ctx context.Context, userID uuid.UUID) (AccountClosureAssessment, error) {
	endpoint := *c.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/internal/v1/users/" + userID.String() + "/account-closure-assessment"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return AccountClosureAssessment{}, err
	}
	request.Header.Set("Authorization", "Bearer "+c.serviceToken)
	response, err := c.client.Do(request)
	if err != nil {
		return AccountClosureAssessment{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return AccountClosureAssessment{}, err
	}
	if response.StatusCode != http.StatusOK {
		return AccountClosureAssessment{}, fmt.Errorf("%s account closure assessment returned HTTP %d", c.domain, response.StatusCode)
	}
	var assessment AccountClosureAssessment
	if err := json.Unmarshal(body, &assessment); err != nil || assessment.UserID != userID {
		return AccountClosureAssessment{}, fmt.Errorf("%s account closure assessment returned invalid data", c.domain)
	}
	for index := range assessment.Blockers {
		assessment.Blockers[index].Domain = c.domain
	}
	assessment.Ready = len(assessment.Blockers) == 0
	return assessment, nil
}
