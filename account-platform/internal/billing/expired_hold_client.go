package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/holdexpiry"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type ExpiredHoldIdentity = holdexpiry.Identity
type ExpiredHoldProof = holdexpiry.Proof
type ExpiredHoldProofClient interface {
	FenceExpiredHold(context.Context, ExpiredHoldIdentity) (ExpiredHoldProof, error)
}
type HTTPExpiredHoldProofClient struct {
	base       string
	credential string
	client     *http.Client
}

func NewHTTPExpiredHoldProofClient(base, credential string, client *http.Client) (*HTTPExpiredHoldProofClient, error) {
	endpoint, err := url.Parse(base)
	if err != nil || endpoint == nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.RawPath != "" || endpoint.Opaque != "" || endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/") || client == nil || strings.TrimSpace(credential) == "" {
		return nil, errors.New("hold proof requires explicit HTTPS workload transport")
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &HTTPExpiredHoldProofClient{base: strings.TrimRight(base, "/"), credential: credential, client: &copyClient}, nil
}
func (c *HTTPExpiredHoldProofClient) call(ctx context.Context, path string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if err = serviceauth.SetAuthorization(req, c.credential); err != nil {
		return err
	}
	res, err := c.client.Do(req)
	if err != nil {
		return errors.New("hold proof transport unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return errors.New("hold dispatch is unproven")
	}
	if out == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(res.Body, 65537))
		return err
	}
	raw, err = io.ReadAll(io.LimitReader(res.Body, 65537))
	if err != nil || len(raw) > 65536 {
		return errors.New("hold proof response invalid")
	}
	return json.Unmarshal(raw, out)
}
func (c *HTTPExpiredHoldProofClient) FenceExpiredHold(ctx context.Context, i ExpiredHoldIdentity) (ExpiredHoldProof, error) {
	var p ExpiredHoldProof
	err := c.call(ctx, "/internal/v1/holds/expiry-proof", i, &p)
	if err == nil && !p.Matches(i) {
		err = errors.New("hold proof binding mismatch")
	}
	return p, err
}
