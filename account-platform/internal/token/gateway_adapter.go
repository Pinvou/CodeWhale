package token

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
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

type ProviderCancelCapability string

const (
	ProviderCancelExplicit             ProviderCancelCapability = "EXPLICIT_CANCEL"
	ProviderCancelStreamAbortConfirmed ProviderCancelCapability = "STREAM_ABORT_CONFIRMED_CANCEL"
	ProviderCancelUnsupported          ProviderCancelCapability = "UNSUPPORTED"
)

type GatewayProtocolCapability struct {
	ProtocolCode    string `json:"protocol_code"`
	ProtocolVersion int64  `json:"protocol_version"`
	Path            string `json:"path"`
	Streaming       bool   `json:"streaming"`
}

type ModelCatalogAdapter interface {
	DiscoverModels(context.Context, string, []byte) ([]DiscoveredModel, error)
}

type HTTPUpstreamClient struct {
	client           *http.Client
	adapterVersion   string
	requestPath      string
	referenceHeaders []string
}

func NewHTTPUpstreamClient() *HTTPUpstreamClient {
	return newHTTPUpstreamClient("openai-compatible-v1", []string{"X-Request-ID", "Request-ID"})
}

func NewNewAPIGatewayAdapter() *HTTPUpstreamClient {
	return newHTTPUpstreamClient("new-api-v1", []string{"X-Oneapi-Request-Id"})
}

func newHTTPUpstreamClient(adapterVersion string, referenceHeaders []string) *HTTPUpstreamClient {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	return &HTTPUpstreamClient{client: &http.Client{Transport: observability.NewHTTPTransport(transport), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, adapterVersion: adapterVersion, requestPath: "/v1/chat/completions", referenceHeaders: referenceHeaders}
}

func (c *HTTPUpstreamClient) AdapterVersion() string { return c.adapterVersion }

// The currently supported OpenAI-compatible and New API adapters do not expose a
// provider-guaranteed idempotent cancellation API. Closing the client stream is
// therefore delivery-only and must never be represented as provider cancellation.
func (c *HTTPUpstreamClient) CancelCapability() ProviderCancelCapability {
	return ProviderCancelUnsupported
}

func (c *HTTPUpstreamClient) Protocols() []GatewayProtocolCapability {
	return []GatewayProtocolCapability{{ProtocolCode: "CHAT", ProtocolVersion: 1, Path: "/chat/completions", Streaming: true}}
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

func parseUsageJSON(payload []byte) (Usage, bool) {
	var body struct {
		Usage *struct {
			PromptTokens       int64 `json:"prompt_tokens"`
			CompletionTokens   int64 `json:"completion_tokens"`
			PromptTokenDetails *struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(payload, &body) != nil || body.Usage == nil {
		return Usage{}, false
	}
	usage := Usage{InputTokens: body.Usage.PromptTokens, OutputTokens: body.Usage.CompletionTokens}
	if body.Usage.PromptTokenDetails != nil {
		usage.CachedInputTokens = body.Usage.PromptTokenDetails.CachedTokens
	}
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.CachedInputTokens < 0 || usage.CachedInputTokens > usage.InputTokens {
		return Usage{}, false
	}
	usage.InputTokens -= usage.CachedInputTokens
	return usage, true
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

func (c *HTTPUpstreamClient) DiscoverModels(ctx context.Context, baseURL string, secret []byte) ([]DiscoveredModel, error) {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || len(secret) == 0 {
		return nil, ErrNoEligibleRoute
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String()+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+string(secret))
	request.Header.Set("Accept", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("model catalog discovery returned status %d", response.StatusCode)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4<<20))
	if err := decoder.Decode(&payload); err != nil {
		return nil, err
	}
	models := make([]DiscoveredModel, 0, len(payload.Data))
	seen := map[string]bool{}
	for _, entry := range payload.Data {
		model := strings.TrimSpace(entry.ID)
		if model != "" && !seen[model] {
			seen[model] = true
			models = append(models, DiscoveredModel{UpstreamModelRef: model, DisplayName: model})
		}
	}
	sort.Slice(models, func(i, j int) bool { return models[i].UpstreamModelRef < models[j].UpstreamModelRef })
	return models, nil
}
