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
	CreateMobileUser(context.Context, MobileRegistration, string) (string, bool, error)
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
		if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
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
	client := observability.NewHTTPClient(10 * time.Second)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &KeycloakProvisioner{adminURL: admin, tokenURL: token, realm: strings.TrimSpace(realm), clientID: strings.TrimSpace(clientID), clientSecret: strings.TrimSpace(clientSecret), client: client}, nil
}

// Registration recovery binds only the opaque intent created by Account. It
// never adopts an existing username/phone or resets an already-created password.
func (p *KeycloakProvisioner) CreateMobileUser(ctx context.Context, registration MobileRegistration, password string) (string, bool, error) {
	access, err := p.accessToken(ctx)
	if err != nil {
		return "", false, err
	}
	if subject, err := p.registrationSubject(ctx, access, registration); err != nil || subject != "" {
		return subject, subject != "", err
	}
	body, _ := json.Marshal(map[string]any{
		"username": registration.Username, "enabled": true,
		"attributes":  map[string][]string{"phone_number": {registration.Mobile}, "phone_number_verified": {"true"}, "pinvou_registration_id": {registration.UserID.String()}},
		"credentials": []map[string]any{{"type": "password", "value": password, "temporary": false}},
	})
	endpoint := *p.adminURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/admin/realms/" + url.PathEscape(p.realm) + "/users"
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+access)
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		// A failed response does not prove that remote creation failed. A bounded
		// lookup of this exact intent may recover an accepted request.
		if subject, lookupErr := p.registrationSubject(ctx, access, registration); lookupErr == nil && subject != "" {
			return subject, true, nil
		}
		return "", false, errors.New("Keycloak user creation is unavailable")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	if response.StatusCode == http.StatusConflict {
		if subject, err := p.registrationSubject(ctx, access, registration); err != nil || subject != "" {
			return subject, subject != "", err
		}
		return "", false, ErrUsernameAlreadyRegistered
	}
	if response.StatusCode != http.StatusCreated {
		return "", false, errors.New("Keycloak user creation was rejected")
	}
	location := response.Header.Get("Location")
	subject := location[strings.LastIndex(location, "/")+1:]
	if subject == "" || len(subject) > 128 {
		return "", false, errors.New("Keycloak user creation omitted the user identifier")
	}
	verified, err := p.registrationSubject(ctx, access, registration)
	if err != nil || verified != subject {
		return "", false, errors.New("Keycloak registration identity could not be verified")
	}
	return subject, false, nil
}

func (p *KeycloakProvisioner) registrationSubject(ctx context.Context, access string, registration MobileRegistration) (string, error) {
	endpoint := *p.adminURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/admin/realms/" + url.PathEscape(p.realm) + "/users"
	endpoint.RawQuery = url.Values{"q": {"pinvou_registration_id:" + registration.UserID.String()}, "max": {"2"}}.Encode()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	request.Header.Set("Authorization", "Bearer "+access)
	response, err := p.client.Do(request)
	if err != nil {
		return "", errors.New("Keycloak registration lookup is unavailable")
	}
	defer response.Body.Close()
	var users []struct {
		ID         string              `json:"id"`
		Username   string              `json:"username"`
		Enabled    bool                `json:"enabled"`
		Attributes map[string][]string `json:"attributes"`
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (256<<10)+1))
	if err != nil || response.StatusCode != http.StatusOK || len(data) > 256<<10 || json.Unmarshal(data, &users) != nil || users == nil {
		return "", errors.New("Keycloak registration lookup failed")
	}
	if len(users) == 0 {
		return "", nil
	}
	if len(users) != 1 {
		return "", ErrConflict
	}
	user := users[0]
	attribute := func(name, value string) bool {
		values := user.Attributes[name]
		return len(values) == 1 && values[0] == value
	}
	if user.ID == "" || len(user.ID) > 128 || !user.Enabled || user.Username != registration.Username ||
		!attribute("pinvou_registration_id", registration.UserID.String()) || !attribute("phone_number", registration.Mobile) || !attribute("phone_number_verified", "true") {
		return "", ErrConflict
	}
	return user.ID, nil
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
	data, readErr := io.ReadAll(io.LimitReader(response.Body, (256<<10)+1))
	if response.StatusCode != http.StatusOK || readErr != nil || len(data) > 256<<10 || json.Unmarshal(data, &body) != nil || body.AccessToken == "" || body.ExpiresIn <= 0 || body.ExpiresIn > int64((time.Duration(1<<63-1))/time.Second) {
		return "", errors.New("Keycloak provisioner authentication failed")
	}
	p.token = body.AccessToken
	p.expiresAt = time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)
	return p.token, nil
}
