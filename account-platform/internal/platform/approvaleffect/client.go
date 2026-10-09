package approvaleffect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

var ErrUnavailable = errors.New("approval effect authority is unavailable")

const maximumResponseBytes = 16 << 10

type Reader interface {
	Observe(context.Context, uuid.UUID, string, string) (Observation, error)
}

type Client struct {
	billingEndpoint *url.URL
	tokenEndpoint   *url.URL
	serviceToken    string
	httpClient      *http.Client
}

// NewClientWithHTTPClient injects the caller's isolated trust configuration.
// The caller must keep that configuration immutable for this client's lifetime.
func NewClientWithHTTPClient(baseURL, credential string, httpClient *http.Client) (*Client, error) {
	if httpClient == nil {
		return nil, ErrUnavailable
	}
	client, err := NewClient(baseURL, credential)
	if err != nil {
		return nil, err
	}
	client.httpClient = httpClient
	return client, nil
}

func NewClient(billingBaseURL, serviceToken string) (*Client, error) {
	billingEndpoint, err := effectEndpoint(billingBaseURL)
	if err != nil {
		return nil, ErrUnavailable
	}
	if strings.TrimSpace(serviceToken) == "" {
		return nil, ErrUnavailable
	}
	client := observability.NewHTTPClient(5 * time.Second)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{billingEndpoint: billingEndpoint, serviceToken: serviceToken, httpClient: client}, nil
}

func effectEndpoint(baseURL string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, ErrUnavailable
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/internal/v1/approval-effects/"
	parsed.RawPath = ""
	return parsed, nil
}

func (c *Client) Observe(ctx context.Context, requestID uuid.UUID, commandType, commandFingerprint string) (Observation, error) {
	if c == nil || c.httpClient == nil || requestID == uuid.Nil || !ValidFingerprint(commandFingerprint) {
		return Observation{}, ErrUnavailable
	}
	authority, known := AuthorityForCommand(commandType)
	if !known {
		return Observation{}, ErrUnavailable
	}
	base := c.billingEndpoint
	if authority == AuthorityToken {
		base = c.tokenEndpoint
	}
	if base == nil {
		return Observation{}, ErrUnavailable
	}
	endpoint := *base
	endpoint.Path += requestID.String()
	query := endpoint.Query()
	query.Set("command_type", commandType)
	query.Set("command_fingerprint", commandFingerprint)
	endpoint.RawQuery = query.Encode()
	requestContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, endpoint.String(), nil)
	if err != nil || serviceauth.SetAuthorization(request, c.serviceToken) != nil {
		return Observation{}, ErrUnavailable
	}
	request.Header.Set("Cache-Control", "no-cache")
	startedAt := time.Now().UTC()
	response, err := c.httpClient.Do(request)
	if err != nil {
		return Observation{}, ErrUnavailable
	}
	defer response.Body.Close()
	cacheControl := response.Header.Values("Cache-Control")
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != http.StatusOK || len(cacheControl) != 1 || cacheControl[0] != "no-store" || mediaErr != nil || mediaType != "application/json" {
		return Observation{}, ErrUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maximumResponseBytes+1))
	if err != nil || len(raw) > maximumResponseBytes || !utf8.Valid(raw) {
		return Observation{}, ErrUnavailable
	}
	if _, err = closedObservation(raw); err != nil {
		return Observation{}, ErrUnavailable
	}
	var observation Observation
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&observation) != nil || decoder.Decode(new(any)) != io.EOF || observation.Validate() != nil ||
		observation.Authority != authority || observation.ApprovalRequestID != requestID || observation.CommandType != commandType ||
		observation.CommandFingerprint != commandFingerprint || observation.ObservedAt.Before(startedAt.Add(-30*time.Second)) ||
		observation.ObservedAt.After(time.Now().UTC().Add(30*time.Second)) {
		return Observation{}, ErrUnavailable
	}
	return observation, nil
}

func closedObservation(raw []byte) (map[string]json.RawMessage, error) {
	allowed := map[string]bool{
		"schema_version": false, "authority": false, "approval_request_id": false, "command_type": false,
		"command_fingerprint": false, "state": false, "observed_at": false,
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, ErrUnavailable
	}
	values := make(map[string]json.RawMessage, len(allowed))
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		seen, known := allowed[key]
		if err != nil || !ok || !known || seen {
			return nil, ErrUnavailable
		}
		allowed[key] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, ErrUnavailable
		}
		values[key] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') || decoder.Decode(new(any)) != io.EOF {
		return nil, ErrUnavailable
	}
	for _, seen := range allowed {
		if !seen {
			return nil, ErrUnavailable
		}
	}
	return values, nil
}

func NewClientWithToken(billingURL, tokenURL, credential string) (*Client, error) {
	c, err := NewClient(billingURL, credential)
	if err != nil {
		return nil, err
	}
	token, err := effectEndpoint(tokenURL)
	if err != nil {
		return nil, err
	}
	c.tokenEndpoint = token
	return c, nil
}
