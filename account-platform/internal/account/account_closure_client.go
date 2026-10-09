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
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
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

type AccountClosureGrantClient interface {
	IsolateGrants(context.Context, uuid.UUID, uuid.UUID, int64) error
}

type AccountClosureReplayClient interface {
	ClearReplay(context.Context, uuid.UUID) error
}

type HTTPAccountClosureClient struct {
	baseURL      *url.URL
	serviceToken string
	domain       string
	client       *http.Client
}

func NewHTTPAccountClosureClient(rawURL, serviceToken, domain string) (*HTTPAccountClosureClient, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
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
	if err := serviceauth.SetAuthorization(request, c.serviceToken); err != nil {
		return AccountClosureAssessment{}, err
	}
	response, err := c.client.Do(request)
	if err != nil {
		return AccountClosureAssessment{}, errors.New("account closure assessment transport unavailable")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return AccountClosureAssessment{}, errors.New("account closure assessment body is invalid")
	}
	if response.StatusCode != http.StatusOK {
		return AccountClosureAssessment{}, fmt.Errorf("%s account closure assessment returned HTTP %d", c.domain, response.StatusCode)
	}
	var wire struct {
		UserID   uuid.UUID                `json:"user_id"`
		Ready    *bool                    `json:"ready"`
		Blockers *[]AccountClosureBlocker `json:"blockers"`
	}
	if err := json.Unmarshal(body, &wire); err != nil || wire.UserID != userID || userID == uuid.Nil || wire.Ready == nil || wire.Blockers == nil || *wire.Ready != (len(*wire.Blockers) == 0) {
		return AccountClosureAssessment{}, fmt.Errorf("%s account closure assessment returned invalid data", c.domain)
	}
	for _, blocker := range *wire.Blockers {
		if blocker.Domain != c.domain || strings.TrimSpace(blocker.Code) == "" || strings.TrimSpace(blocker.ResourceType) == "" {
			return AccountClosureAssessment{}, errors.New("account closure blocker authority is invalid")
		}
	}
	assessment := AccountClosureAssessment{UserID: wire.UserID, Ready: *wire.Ready, Blockers: *wire.Blockers}
	return assessment, nil
}

func (c *HTTPAccountClosureClient) IsolateGrants(ctx context.Context, userID, closureID uuid.UUID, lifecycleVersion int64) error {
	if c.domain != "BILLING" || userID == uuid.Nil || closureID == uuid.Nil || lifecycleVersion < 2 {
		return errors.New("billing grant isolation request is invalid")
	}
	endpoint := *c.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/internal/v1/users/" + userID.String() + "/account-closure-grants/isolate"
	payload, _ := json.Marshal(map[string]any{"closure_request_id": closureID, "subject_lifecycle_version": lifecycleVersion})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	if err := serviceauth.SetAuthorization(request, c.serviceToken); err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return errors.New("billing grant isolation transport unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("billing account closure grant isolation returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return errors.New("billing grant isolation receipt is invalid")
	}
	var receipt struct {
		ClosureID uuid.UUID `json:"closure_request_id"`
		UserID    uuid.UUID `json:"member_user_id"`
		Lifecycle int64     `json:"subject_lifecycle_version"`
		Count     *int      `json:"isolated_grant_count"`
		Replayed  *bool     `json:"replayed"`
	}
	if json.Unmarshal(body, &receipt) != nil || receipt.ClosureID != closureID || receipt.UserID != userID || receipt.Lifecycle != lifecycleVersion || receipt.Count == nil || *receipt.Count < 0 || receipt.Replayed == nil {
		return errors.New("billing grant isolation receipt does not match the closure")
	}
	return nil
}

func (c *HTTPAccountClosureClient) ClearReplay(ctx context.Context, userID uuid.UUID) error {
	if c.domain != "TOKEN" || userID == uuid.Nil {
		return errors.New("token replay cleanup request is invalid")
	}
	endpoint := *c.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/internal/v1/users/" + userID.String() + "/replay-payloads"
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint.String(), nil)
	if err != nil {
		return errors.New("invalid token replay cleanup request")
	}
	if err := serviceauth.SetAuthorization(request, c.serviceToken); err != nil {
		return err
	}
	response, err := c.client.Do(request)
	if err != nil {
		return errors.New("token replay cleanup transport unavailable")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(body) != 0 || response.StatusCode != http.StatusNoContent {
		return errors.New("token replay cleanup acknowledgement is invalid")
	}
	return nil
}
