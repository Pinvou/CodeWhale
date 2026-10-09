package token

import (
	"bytes"
	"context"

	"net/http"
	"net/url"

	"strings"

	"github.com/google/uuid"
)

type UpstreamClient interface {
	Do(context.Context, string, []byte, []byte, uuid.UUID) (*http.Response, error)
	AdapterVersion() string
	Protocols() []GatewayProtocolCapability
	UpstreamRequestID(*http.Response) string
	ParseUsage([]byte) (Usage, bool)
	RejectsBeforeGeneration(int) bool
	CopyResponseHeaders(http.Header, http.Header)
	CancelCapability() ProviderCancelCapability
}

func (c *HTTPUpstreamClient) UpstreamRequestID(response *http.Response) string {
	if response == nil {
		return ""
	}
	for _, header := range c.referenceHeaders {
		if value := strings.TrimSpace(response.Header.Get(header)); value != "" {
			return value
		}
	}
	return ""
}

func (c *HTTPUpstreamClient) ParseUsage(payload []byte) (Usage, bool) {
	return parseUsageJSON(payload)
}

func (c *HTTPUpstreamClient) RejectsBeforeGeneration(statusCode int) bool {
	switch statusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}

func (c *HTTPUpstreamClient) CopyResponseHeaders(destination, source http.Header) {
	for key, values := range source {
		blocked := false
		for _, referenceHeader := range c.referenceHeaders {
			if strings.EqualFold(key, referenceHeader) {
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}
		switch strings.ToLower(key) {
		case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade", "set-cookie", "content-length":
			continue
		}
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}

func (c *HTTPUpstreamClient) Do(ctx context.Context, baseURL string, secret, body []byte, usageID uuid.UUID) (*http.Response, error) {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, ErrNoEligibleRoute
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, parsed.String()+c.requestPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+string(secret))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("X-Request-ID", usageID.String())
	return c.client.Do(request)
}
