package paymentcredit

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
)

type HTTPClient struct {
	baseURL    *url.URL
	credential string
	client     *http.Client
}

func NewHTTPClient(rawURL, credential string) (*HTTPClient, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.RawPath != "" || parsed.Fragment != "" || strings.TrimSpace(credential) == "" {
		return nil, ErrProtocol
	}
	client := observability.NewHTTPClient(5 * time.Second)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &HTTPClient{baseURL: parsed, credential: strings.TrimSpace(credential), client: client}, nil
}

func (c *HTTPClient) Admit(ctx context.Context, input Request) (Decision, error) {
	decision, err := c.call(ctx, "/internal/v1/payment-credit-admissions", input)
	if err != nil {
		return Decision{}, err
	}
	switch decision.State {
	case "ADMITTED", "CLOSED", "SUBJECT_UNAVAILABLE", "APPLIED", "ABORTED":
		return decision, nil
	default:
		return Decision{}, ErrProtocol
	}
}

func (c *HTTPClient) Resolve(ctx context.Context, input Request) (Decision, error) {
	decision, err := c.call(ctx, "/internal/v1/payment-credit-attempts/resolve", input)
	if err != nil {
		return Decision{}, err
	}
	if decision.State != "APPLIED" && decision.State != "ABORTED" {
		return Decision{}, ErrProtocol
	}
	return decision, nil
}

func (c *HTTPClient) Acknowledge(ctx context.Context, input Request, state string) (Decision, error) {
	if state != "APPLIED" && state != "ABORTED" {
		return Decision{}, ErrProtocol
	}
	decision, err := c.send(ctx, "/internal/v1/payment-credit-attempts/acknowledge", input, Acknowledgement{Request: input, State: state})
	if err != nil {
		return Decision{}, err
	}
	if decision.State != state {
		return Decision{}, ErrProtocol
	}
	return decision, nil
}

func (c *HTTPClient) call(ctx context.Context, path string, input Request) (Decision, error) {
	return c.send(ctx, path, input, input)
}

func (c *HTTPClient) send(ctx context.Context, path string, input Request, payload any) (Decision, error) {
	if !input.Valid() {
		return Decision{}, ErrProtocol
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Decision{}, ErrProtocol
	}
	endpoint := *c.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + path
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return Decision{}, ErrProtocol
	}
	request.Header.Set("Content-Type", "application/json")
	if err = serviceauth.SetAuthorization(request, c.credential); err != nil {
		return Decision{}, ErrProtocol
	}
	response, err := c.client.Do(request)
	if err != nil {
		return Decision{}, ErrProtocol
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return Decision{}, ErrProtocol
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(raw) > 4096 {
		return Decision{}, ErrProtocol
	}
	// The response is flat; reject duplicate and unknown fields explicitly.
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return Decision{}, ErrProtocol
	}
	seen := map[string]bool{}
	decision := Decision{}
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok || seen[name] {
			return Decision{}, ErrProtocol
		}
		seen[name] = true
		switch name {
		case "id":
			err = decoder.Decode(&decision.ID)
		case "fingerprint":
			err = decoder.Decode(&decision.Fingerprint)
		case "state":
			err = decoder.Decode(&decision.State)
		default:
			return Decision{}, ErrProtocol
		}
		if err != nil {
			return Decision{}, ErrProtocol
		}
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		return Decision{}, ErrProtocol
	}
	if _, err = decoder.Token(); err != io.EOF || !decision.Matches(input) || len(seen) != 3 {
		return Decision{}, ErrProtocol
	}
	return decision, nil
}
