package usagewaiver

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
)

type Client struct {
	base       *url.URL
	credential string
	http       *http.Client
}
type HTTPError struct{ Status int }

func (e *HTTPError) Error() string { return fmt.Sprintf("waiver authority returned HTTP %d", e.Status) }

func NewClient(rawURL, credential string, client *http.Client) (*Client, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.TrimSpace(credential) == "" {
		return nil, errors.New("invalid usage waiver authority configuration")
	}
	if client == nil {
		client = observability.NewHTTPClient(10 * time.Second)
	}
	isolated := *client
	isolated.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{base: parsed, credential: credential, http: &isolated}, nil
}

// Call decodes the real authority's exact versioned contract. A missing or
// malformed acknowledgement is unknown; callers retain their original intent.
func (c *Client) Call(ctx context.Context, path string, input any, output any) error {
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base.String()+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	if err := serviceauth.SetAuthorization(req, c.credential); err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return &HTTPError{Status: response.StatusCode}
	}
	reader := io.LimitReader(response.Body, (2<<20)+1)
	body, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	if len(body) > 2<<20 {
		return errors.New("usage waiver authority response too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("usage waiver authority response has trailing data")
	}
	return nil
}
