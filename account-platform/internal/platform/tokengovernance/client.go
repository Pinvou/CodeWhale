// Package tokengovernance defines the narrow cross-service activation read
// contract. It has no domain storage or policy mutation capability.
package tokengovernance

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
)

var ErrUnavailable = errors.New("Token governance activation is unavailable")

type Snapshot struct {
	State                string    `json:"state"`
	Epoch                int64     `json:"epoch"`
	PolicySetDigest      string    `json:"policy_set_digest"`
	HandlerSetDigest     string    `json:"handler_set_digest"`
	ApprovedRuntimeRoles []string  `json:"approved_runtime_roles"`
	UpdatedAt            time.Time `json:"updated_at"`
}

type Reader interface {
	Snapshot(context.Context) (Snapshot, error)
}

type Client struct {
	endpoint      string
	serviceToken  string
	policyDigest  string
	handlerDigest string
	client        *http.Client
}

func NewClient(baseURL, serviceToken, policyDigest, handlerDigest string) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") || !validDigest(policyDigest) || !validDigest(handlerDigest) {
		return nil, ErrUnavailable
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/internal/v1/token-governance/activation"
	client := observability.NewHTTPClient(5 * time.Second)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{endpoint: u.String(), serviceToken: serviceToken, policyDigest: policyDigest, handlerDigest: handlerDigest, client: client}, nil
}

func (c *Client) Snapshot(ctx context.Context) (Snapshot, error) {
	var value Snapshot
	if c == nil || c.client == nil {
		return value, ErrUnavailable
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint, nil)
	if err != nil || serviceauth.SetAuthorization(request, c.serviceToken) != nil {
		return value, ErrUnavailable
	}
	request.Header.Set("Cache-Control", "no-cache")
	response, err := c.client.Do(request)
	if err != nil {
		return value, ErrUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
		return value, ErrUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (16<<10)+1))
	if err != nil || len(raw) > 16<<10 {
		return Snapshot{}, ErrUnavailable
	}
	keys := map[string]bool{"state": false, "epoch": false, "policy_set_digest": false, "handler_set_digest": false, "approved_runtime_roles": false, "updated_at": false}
	shape := json.NewDecoder(bytes.NewReader(raw))
	if token, err := shape.Token(); err != nil || token != json.Delim('{') {
		return Snapshot{}, ErrUnavailable
	}
	for shape.More() {
		token, err := shape.Token()
		key, ok := token.(string)
		seen, known := keys[key]
		if err != nil || !ok || !known || seen {
			return Snapshot{}, ErrUnavailable
		}
		keys[key] = true
		var value json.RawMessage
		if shape.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return Snapshot{}, ErrUnavailable
		}
	}
	if token, err := shape.Token(); err != nil || token != json.Delim('}') || shape.Decode(new(any)) != io.EOF {
		return Snapshot{}, ErrUnavailable
	}
	for _, seen := range keys {
		if !seen {
			return Snapshot{}, ErrUnavailable
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF || value.UpdatedAt.IsZero() || value.UpdatedAt.After(time.Now().Add(time.Second)) {
		return Snapshot{}, ErrUnavailable
	}
	if value.State == "PRE_ACTIVATION" && value.Epoch == 0 && value.PolicySetDigest == "" && value.HandlerSetDigest == "" && len(value.ApprovedRuntimeRoles) == 0 {
		return value, nil
	}
	if (value.State != "ACTIVE" && value.State != "SAFE_ROLLBACK") || value.Epoch < 1 || value.PolicySetDigest != c.policyDigest || value.HandlerSetDigest != c.handlerDigest || len(value.ApprovedRuntimeRoles) == 0 || !slices.IsSorted(value.ApprovedRuntimeRoles) {
		return Snapshot{}, ErrUnavailable
	}
	for index, role := range value.ApprovedRuntimeRoles {
		if role == "" || strings.TrimSpace(role) != role || (index > 0 && role == value.ApprovedRuntimeRoles[index-1]) {
			return Snapshot{}, ErrUnavailable
		}
	}
	return value, nil
}

func validDigest(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
