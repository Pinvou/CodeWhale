package workload

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

const maximumResponse = 64 * 1024
const maximumToken = 32 * 1024
const minimumRemaining = 30 * time.Second

type Agent struct {
	config   Config
	client   *http.Client
	verifier *oidc.IDTokenVerifier
	now      func() time.Time
}

func New(config Config) (*Agent, error) {
	return newAgent(config, http.DefaultTransport.(*http.Transport).Clone(), time.Now)
}

// The transport injection is package-private for TLS fixtures; production has
// no insecure switch, endpoint override, or caller-supplied verifier.
func newAgent(config Config, transport http.RoundTripper, now func() time.Time) (*Agent, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	client := &http.Client{
		Timeout:       10 * time.Second,
		Transport:     boundedTransport{base: transport},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	keySet := oidc.NewRemoteKeySet(oidc.ClientContext(context.Background(), client), config.Issuer+"/protocol/openid-connect/certs")
	verifier := oidc.NewVerifier(config.Issuer, keySet, &oidc.Config{
		ClientID: config.Audience, SupportedSigningAlgs: []string{oidc.RS256}, Now: now,
	})
	return &Agent{config: config, client: client, verifier: verifier, now: now}, nil
}

// Refresh returns only verified expiry, never tokens or remote diagnostic data.
func (a *Agent) Refresh(ctx context.Context) (time.Time, error) {
	secret, err := readPrivateFile(a.config.ClientSecretFile, 4096)
	if err != nil {
		return time.Time{}, errors.New("workload bootstrap credential unavailable")
	}
	defer clear(secret)
	credential := strings.TrimSpace(string(secret))
	if len(credential) < 16 || strings.ContainsAny(credential, " \t\r\n") {
		return time.Time{}, errors.New("workload bootstrap credential invalid")
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {a.config.ClientID}, "client_secret": {credential}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.config.Issuer+"/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		return time.Time{}, errors.New("workload request construction failed")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := a.client.Do(request)
	if err != nil {
		return time.Time{}, errors.New("workload token endpoint unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return time.Time{}, errors.New("workload token endpoint rejected request")
	}
	var result struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	decoder := json.NewDecoder(response.Body)
	if err := decoder.Decode(&result); err != nil || decoder.Decode(new(any)) != io.EOF || !strings.EqualFold(result.TokenType, "Bearer") {
		return time.Time{}, errors.New("workload token response invalid")
	}
	expiry, err := a.verify(ctx, result.AccessToken)
	if err != nil {
		return time.Time{}, err
	}
	if err := publish(a.config.TokenFile, []byte(result.AccessToken)); err != nil {
		return time.Time{}, err
	}
	return expiry, nil
}

func (a *Agent) verify(ctx context.Context, raw string) (time.Time, error) {
	if len(raw) == 0 || len(raw) > maximumToken || strings.ContainsAny(raw, " \t\r\n") {
		return time.Time{}, errors.New("workload token format invalid")
	}
	token, err := a.verifier.Verify(ctx, raw)
	if err != nil {
		return time.Time{}, errors.New("workload token verification failed")
	}
	var claims struct {
		AuthorizedParty string `json:"azp"`
		NotBefore       int64  `json:"nbf"`
	}
	now := a.now()
	lifetime := token.Expiry.Sub(token.IssuedAt)
	if err := token.Claims(&claims); err != nil || claims.AuthorizedParty != a.config.ClientID || token.Subject != a.config.Subject || token.IssuedAt.IsZero() || token.IssuedAt.After(now) || lifetime <= 0 || lifetime > 15*time.Minute || token.Expiry.Sub(now) < minimumRemaining || (claims.NotBefore != 0 && time.Unix(claims.NotBefore, 0).After(now)) {
		return time.Time{}, errors.New("workload token identity or lifetime invalid")
	}
	return token.Expiry, nil
}

func (a *Agent) Check(ctx context.Context) error {
	body, err := readPrivateFile(a.config.TokenFile, maximumToken)
	if err != nil {
		return errors.New("workload projected token unavailable")
	}
	defer clear(body)
	_, err = a.verify(ctx, string(body))
	return err
}

// Run is single-writer. Failure preserves the old file, not its validity: the
// receiving service must still verify JWT expiry on every request.
func (a *Agent) Run(ctx context.Context, report func(string)) error {
	retry := 5 * time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		expiry, err := a.Refresh(ctx)
		var wait time.Duration
		if err != nil {
			if report != nil {
				report("workload refresh failed; retry scheduled")
			}
			wait = retry
			retry = min(retry*2, 30*time.Second)
		} else {
			if report != nil {
				report("workload token renewed")
			}
			retry = 5 * time.Second
			wait = max(time.Second, expiry.Sub(a.now())/2)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// Bound both token and JWKS bodies, including non-2xx diagnostics, before any
// parser sees them. Raw errors never escape Agent's stable error boundaries.
type boundedTransport struct{ base http.RoundTripper }

func (t boundedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maximumResponse+1))
	response.Body.Close()
	if readErr != nil || len(body) > maximumResponse {
		return nil, errors.New("workload response limit exceeded")
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	return response, nil
}
