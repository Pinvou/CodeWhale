package account

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
	"sync"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
)

type IdentityProvisioner interface {
	CreateMobileUser(context.Context, string, string) (string, error)
	DeleteUser(context.Context, string) error
}

type KeycloakProvisioner struct {
	adminURL     *url.URL
	tokenURL     *url.URL
	realm        string
	clientID     string
	clientSecret string
	client       *http.Client

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

func NewKeycloakProvisioner(adminURL, tokenURL, realm, clientID, clientSecret string) (*KeycloakProvisioner, error) {
	parse := func(name, raw string) (*url.URL, error) {
		parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
		if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.User != nil {
			return nil, fmt.Errorf("%s must be an absolute HTTP(S) URL", name)
		}
		return parsed, nil
	}
	admin, err := parse("KEYCLOAK_ADMIN_API_URL", adminURL)
	if err != nil {
		return nil, err
	}
	token, err := parse("KEYCLOAK_PROVISIONER_TOKEN_URL", tokenURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(realm) == "" || strings.TrimSpace(clientID) == "" || strings.TrimSpace(clientSecret) == "" {
		return nil, errors.New("Keycloak provisioner realm and client credentials are required")
	}
	return &KeycloakProvisioner{adminURL: admin, tokenURL: token, realm: strings.TrimSpace(realm), clientID: strings.TrimSpace(clientID), clientSecret: strings.TrimSpace(clientSecret), client: observability.NewHTTPClient(10 * time.Second)}, nil
}

func (p *KeycloakProvisioner) CreateMobileUser(ctx context.Context, mobile, password string) (string, error) {
	access, err := p.accessToken(ctx)
	if err != nil {
		return "", err
	}
	national := strings.TrimPrefix(mobile, "+86")
	body, _ := json.Marshal(map[string]any{
		"username": national, "enabled": true,
		"attributes":  map[string][]string{"phone_number": {mobile}, "phone_number_verified": {"true"}},
		"credentials": []map[string]any{{"type": "password", "value": password, "temporary": false}},
	})
	endpoint := *p.adminURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/admin/realms/" + url.PathEscape(p.realm) + "/users"
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+access)
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("create Keycloak user: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	if response.StatusCode == http.StatusConflict {
		return "", ErrMobileAlreadyRegistered
	}
	if response.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("create Keycloak user returned HTTP %d", response.StatusCode)
	}
	location := response.Header.Get("Location")
	subject := location[strings.LastIndex(location, "/")+1:]
	if subject == "" || len(subject) > 128 {
		return "", errors.New("Keycloak user creation omitted the user identifier")
	}
	return subject, nil
}

func (p *KeycloakProvisioner) DeleteUser(ctx context.Context, subject string) error {
	access, err := p.accessToken(ctx)
	if err != nil {
		return err
	}
	endpoint := *p.adminURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/admin/realms/" + url.PathEscape(p.realm) + "/users/" + url.PathEscape(subject)
	request, _ := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint.String(), nil)
	request.Header.Set("Authorization", "Bearer "+access)
	response, err := p.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusNotFound {
		return fmt.Errorf("delete Keycloak user returned HTTP %d", response.StatusCode)
	}
	return nil
}

func (p *KeycloakProvisioner) accessToken(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.token != "" && time.Now().Before(p.expiresAt.Add(-30*time.Second)) {
		return p.token, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {p.clientID}, "client_secret": {p.clientSecret}}
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenURL.String(), strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := p.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("obtain Keycloak provisioner token: %w", err)
	}
	defer response.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 256<<10)).Decode(&body) != nil || body.AccessToken == "" {
		return "", errors.New("Keycloak provisioner authentication failed")
	}
	p.token = body.AccessToken
	p.expiresAt = time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)
	return p.token, nil
}
