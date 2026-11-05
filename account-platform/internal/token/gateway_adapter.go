package token

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
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

func NewNewAPIGatewayAdapter() *HTTPUpstreamClient {
	return newHTTPUpstreamClient("new-api-v1", []string{"X-Oneapi-Request-Id"})
}

// NewNewAPIGatewayAdapterWithCA adds a provider-specific trust root without
// widening the process-wide trust store used by unrelated HTTP clients.
func NewNewAPIGatewayAdapterWithCA(caFile string) (*HTTPUpstreamClient, error) {
	return newHTTPUpstreamClientWithCA("new-api-v1", []string{"X-Oneapi-Request-Id"}, caFile)
}

func newHTTPUpstreamClient(adapterVersion string, referenceHeaders []string) *HTTPUpstreamClient {
	client, _ := newHTTPUpstreamClientWithCA(adapterVersion, referenceHeaders, "")
	return client
}

func newHTTPUpstreamClientWithCA(adapterVersion string, referenceHeaders []string, caFile string) (*HTTPUpstreamClient, error) {
	transport, err := newProviderHTTPTransport(caFile)
	if err != nil {
		return nil, err
	}
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	return &HTTPUpstreamClient{client: &http.Client{Transport: observability.NewHTTPTransport(transport), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, adapterVersion: adapterVersion, requestPath: "/v1/chat/completions", referenceHeaders: referenceHeaders}, nil
}

func newProviderHTTPTransport(caFile string) (*http.Transport, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	caFile = strings.TrimSpace(caFile)
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read model provider CA: %w", err)
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("model provider CA contains no valid certificates")
		}
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	}
	return transport, nil
}

func (c *HTTPUpstreamClient) AdapterVersion() string { return c.adapterVersion }

func (c *HTTPUpstreamClient) Protocols() []GatewayProtocolCapability {
	return []GatewayProtocolCapability{{ProtocolCode: "CHAT", ProtocolVersion: 1, Path: "/chat/completions", Streaming: true}}
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
		Data *[]struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil || len(body) > 4<<20 {
		return nil, fmt.Errorf("model catalog discovery body is incomplete or too large")
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if payload.Data == nil {
		return nil, fmt.Errorf("model catalog discovery requires a non-null data array")
	}
	models := make([]DiscoveredModel, 0, len(*payload.Data))
	seen := map[string]bool{}
	for _, entry := range *payload.Data {
		model := strings.TrimSpace(entry.ID)
		if model == "" {
			return nil, fmt.Errorf("model catalog discovery contains an invalid model identifier")
		}
		if !seen[model] {
			seen[model] = true
			models = append(models, DiscoveredModel{UpstreamModelRef: model, DisplayName: model})
		}
	}
	sort.Slice(models, func(i, j int) bool { return models[i].UpstreamModelRef < models[j].UpstreamModelRef })
	return models, nil
}
