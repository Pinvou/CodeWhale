package token

import (
	"net/http"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
)

// NewHTTPModelValidatorWithCA uses the same provider-specific trust as catalog
// discovery without changing the trust store of Account/Billing clients.
func NewHTTPModelValidatorWithCA(policy ProviderURLPolicy, caFile string) (*HTTPModelValidator, error) {
	transport, err := newProviderHTTPTransport(caFile)
	if err != nil {
		return nil, err
	}
	return &HTTPModelValidator{policy: policy, client: &http.Client{
		Timeout: 10 * time.Second, Transport: observability.NewHTTPTransport(transport),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func NewOpenAICompatibleGatewayAdapterWithCA(caFile string) (*HTTPUpstreamClient, error) {
	return newHTTPUpstreamClientWithCA("openai-compatible-v1", []string{"X-Request-Id"}, caFile)
}
