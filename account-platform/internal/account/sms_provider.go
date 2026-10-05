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
	"github.com/google/uuid"
)

// SMSProviderConfig keeps authentication independent of a specific SMS vendor.
// HTTP adapters are operator-owned services which deliver and verify codes;
// codes are never returned by the Account API or written to logs.
func newHTTPManagedSMSProvider(config SMSProviderConfig) (SMSProvider, error) {
	endpoint, err := url.Parse(strings.TrimSpace(config.Endpoint))
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Scheme != "https" && !(config.AllowInsecure && endpoint.Scheme == "http")) || len(config.ServiceToken) < 32 {
		return nil, errors.New("SMS HTTP adapter requires an HTTPS endpoint and service token; HTTP is limited to development or test")
	}
	client := observability.NewHTTPClient(10 * time.Second)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &httpSMSProvider{endpoint: endpoint, token: config.ServiceToken, client: client}, nil
}

func (*httpSMSProvider) Descriptor() SMSProviderDescriptor {
	return SMSProviderDescriptor{ID: "http", AdapterVersion: "v1"}
}

type httpSMSProvider struct {
	endpoint *url.URL
	token    string
	client   *http.Client
}

func (p *httpSMSProvider) call(ctx context.Context, action string, input, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return ErrSMSUnavailable
	}
	endpoint := *p.endpoint
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/" + action
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return ErrSMSUnavailable
	}
	request.Header.Set("Authorization", "Bearer "+p.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return ErrSMSUnavailable
	}
	defer response.Body.Close()
	body, err = io.ReadAll(io.LimitReader(response.Body, (256<<10)+1))
	if err != nil || len(body) > 256<<10 || response.StatusCode != http.StatusOK || json.Unmarshal(body, output) != nil {
		// This adapter's protocol has no authoritative rejection response.
		// A non-success response may follow accepted delivery, so keep cooldown.
		return ErrSMSUnavailable
	}
	return nil
}

func (p *httpSMSProvider) SendCode(ctx context.Context, mobile string, purpose SMSPurpose, challengeID uuid.UUID) (SMSDelivery, error) {
	var result struct {
		MessageID string `json:"message_id"`
	}
	err := p.call(ctx, "send", map[string]any{"mobile": mobile, "purpose": purpose, "challenge_id": challengeID}, &result)
	if err != nil {
		return SMSDelivery{}, err
	}
	if result.MessageID == "" || len(result.MessageID) > 128 {
		return SMSDelivery{}, ErrSMSUnavailable
	}
	return SMSDelivery{MessageID: result.MessageID}, nil
}

func (p *httpSMSProvider) VerifyCode(ctx context.Context, mobile string, challengeID uuid.UUID, verification SMSVerificationData, code string) (bool, error) {
	if verification.Provider != p.Descriptor() {
		return false, ErrSMSInvalid
	}
	var result struct {
		Verified *bool `json:"verified"`
	}
	err := p.call(ctx, "verify", map[string]any{"mobile": mobile, "challenge_id": challengeID, "code": code}, &result)
	if err != nil {
		return false, err
	}
	if result.Verified == nil {
		return false, ErrSMSUnavailable
	}
	return *result.Verified, nil
}
