package token

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"io"

	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"

	"github.com/google/uuid"
)

func (c *HTTPBillingClient) Balance(ctx context.Context, walletID uuid.UUID) (json.RawMessage, error) {
	payload, err := c.raw(ctx, http.MethodGet, "/internal/v1/wallets/"+walletID.String()+"/balance")
	if err != nil {
		return nil, err
	}
	if _, err := validateWalletFacts(payload, walletID, uuid.Nil, true); err != nil {
		return nil, err
	}
	return payload, nil
}

type HTTPBillingClient struct {
	baseURL      *url.URL
	serviceToken string
	client       *http.Client
}

func NewHTTPBillingClient(rawURL, serviceToken string) (*HTTPBillingClient, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.TrimSpace(serviceToken) == "" {
		return nil, errors.New("invalid Billing client configuration")
	}
	return &HTTPBillingClient{baseURL: parsed, serviceToken: serviceToken, client: observability.NewHTTPClient(10 * time.Second)}, nil
}

func (c *HTTPBillingClient) raw(ctx context.Context, method, endpointPath string) (json.RawMessage, error) {
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL.String()+endpointPath, nil)
	if err != nil {
		return nil, err
	}
	if err := serviceauth.SetAuthorization(request, c.serviceToken); err != nil {
		return nil, err
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Billing balance returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > 2<<20 || !json.Valid(body) {
		return nil, errors.New("Billing balance response is invalid")
	}
	return json.RawMessage(body), nil
}

// Required authority facts use pointers where zero is a valid monetary value.
// Unknown reply metadata is allowed and successful replies retain their bytes.
type walletFacts struct {
	WalletID              uuid.UUID `json:"wallet_id"`
	OwnerType             string    `json:"owner_type"`
	OwnerID               uuid.UUID `json:"owner_id"`
	Status                string    `json:"status"`
	Version               int64     `json:"version"`
	AvailableMicrocredits *int64    `json:"available_microcredits"`
}

func (facts walletFacts) validIdentity() bool {
	return facts.WalletID != uuid.Nil && facts.OwnerType == "PERSONAL" && facts.OwnerID != uuid.Nil && facts.Version > 0 && (facts.Status == "ACTIVE" || facts.Status == "FROZEN" || facts.Status == "CLOSED")
}

func validateWalletFacts(payload []byte, walletID, ownerID uuid.UUID, requireBalance bool) (walletFacts, error) {
	var facts walletFacts
	if err := json.Unmarshal(payload, &facts); err != nil || walletID == uuid.Nil || !facts.validIdentity() || facts.WalletID != walletID || (ownerID != uuid.Nil && facts.OwnerID != ownerID) || (requireBalance && facts.AvailableMicrocredits == nil) {
		return walletFacts{}, errors.New("Billing returned invalid wallet authority facts")
	}
	return facts, nil
}
