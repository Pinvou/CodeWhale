package identityadapter

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
	"github.com/google/uuid"
)

var ErrUsernameAlreadyRegistered = errors.New("username is already registered")

// EnsureBootstrapAdmin idempotently creates the disabled deployment-owned
// identity used by the secure bootstrap CLI. No credential is assigned here.
func (p *KeycloakDirectory) EnsureBootstrapAdmin(ctx context.Context, username string) (string, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return "", errors.New("bootstrap username is required")
	}
	access, err := p.accessToken(ctx)
	if err != nil {
		return "", err
	}
	endpoint := *p.adminURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/admin/realms/" + url.PathEscape(p.realm) + "/users"
	query := endpoint.Query()
	query.Set("username", username)
	query.Set("exact", "true")
	query.Set("max", "2")
	endpoint.RawQuery = query.Encode()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	request.Header.Set("Authorization", "Bearer "+access)
	response, err := p.client.Do(request)
	if err != nil {
		return "", err
	}
	var users []struct {
		ID         string              `json:"id"`
		Username   string              `json:"username"`
		Attributes map[string][]string `json:"attributes"`
	}
	if response.StatusCode != http.StatusOK || decodeIdentityJSON(response.Body, 128<<10, &users) != nil || users == nil {
		response.Body.Close()
		return "", fmt.Errorf("query bootstrap Keycloak user returned HTTP %d", response.StatusCode)
	}
	response.Body.Close()
	if len(users) == 1 && strings.EqualFold(users[0].Username, username) {
		if strings.TrimSpace(users[0].ID) == "" || len(users[0].ID) > 128 {
			return "", errors.New("bootstrap identity omitted subject")
		}
		managed := users[0].Attributes["pinvou_bootstrap_managed"]
		if len(managed) != 1 || managed[0] != "true" {
			return "", errors.New("existing bootstrap username is not deployment-managed")
		}
		return users[0].ID, nil
	}
	if len(users) > 0 {
		return "", errors.New("bootstrap username is ambiguous")
	}
	body, _ := json.Marshal(map[string]any{
		"username": username,
		"enabled":  false,
		"attributes": map[string][]string{
			"pinvou_bootstrap_managed": {"true"},
		},
	})
	endpoint.RawQuery = ""
	request, _ = http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+access)
	request.Header.Set("Content-Type", "application/json")
	response, err = p.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("create bootstrap Keycloak user returned HTTP %d", response.StatusCode)
	}
	location := response.Header.Get("Location")
	subject := location[strings.LastIndex(location, "/")+1:]
	if subject == "" {
		return "", errors.New("bootstrap Keycloak user omitted subject")
	}
	return subject, nil
}

// ActivateBootstrapAdmin sets the formal password and only then enables the
// identity. The caller keeps the Account activation transaction open until
// this operation and session revocation both succeed.
func (p *KeycloakDirectory) ActivateBootstrapAdmin(ctx context.Context, subject, username, password string) error {
	if err := ValidatePrivilegedPassword(password, username); err != nil {
		return err
	}
	if err := p.SetPassword(ctx, subject, password); err != nil {
		return err
	}
	if err := p.updateUser(ctx, subject, func(user map[string]any) { user["enabled"] = true }); err != nil {
		return err
	}
	return p.RevokeSessions(ctx, subject)
}

type KeycloakDirectory struct {
	adminURL                      *url.URL
	tokenURL                      *url.URL
	realm, clientID, clientSecret string
	client                        *http.Client
	mu                            sync.Mutex
	token                         string
	expiresAt                     time.Time
}

func NewKeycloakDirectory(environment, adminURL, tokenURL, realm, clientID, clientSecret string) (*KeycloakDirectory, error) {
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
	// Preserve the Account service's existing production transport gate after
	// moving password and provisioner credentials to the Identity boundary.
	// APP_ENV defaults to development, as in platform/config.Load.
	environment = strings.TrimSpace(environment)
	if environment == "" {
		environment = "development"
	}
	if environment != "development" && environment != "test" && (admin.Scheme != "https" || token.Scheme != "https") {
		return nil, errors.New("production Keycloak provisioning requires HTTPS endpoints")
	}
	if strings.TrimSpace(realm) == "" || strings.TrimSpace(clientID) == "" || strings.TrimSpace(clientSecret) == "" {
		return nil, errors.New("Keycloak provisioner realm and client credentials are required")
	}
	client := observability.NewHTTPClient(10 * time.Second)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &KeycloakDirectory{adminURL: admin, tokenURL: token, realm: strings.TrimSpace(realm), clientID: strings.TrimSpace(clientID), clientSecret: strings.TrimSpace(clientSecret), client: client}, nil
}

func (p *KeycloakDirectory) CreateMobileUser(ctx context.Context, operationID uuid.UUID, username, mobile, password string, legacyIntent bool) (RegistrationIdentity, error) {
	if operationID == uuid.Nil || strings.TrimSpace(username) == "" || strings.TrimSpace(mobile) == "" {
		return RegistrationIdentity{}, ErrReferenceInvalid
	}
	if identity, found, err := p.FindMobileUserByRegistrationOperation(ctx, operationID, legacyIntent); err != nil || found {
		if err != nil {
			return RegistrationIdentity{}, err
		}
		if !identity.matches(operationID, username, mobile) {
			return RegistrationIdentity{}, ErrReferenceConflict
		}
		return identity, nil
	}
	access, err := p.accessToken(ctx)
	if err != nil {
		return RegistrationIdentity{}, err
	}
	body, _ := json.Marshal(map[string]any{
		"username": username, "enabled": true,
		"attributes": map[string][]string{
			"phone_number": {mobile}, "phone_number_verified": {"true"},
			registrationMarker(legacyIntent): {operationID.String()},
		},
		"credentials": []map[string]any{{"type": "password", "value": password, "temporary": false}},
	})
	endpoint := *p.adminURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/admin/realms/" + url.PathEscape(p.realm) + "/users"
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+access)
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		if identity, found, lookupErr := p.FindMobileUserByRegistrationOperation(ctx, operationID, legacyIntent); lookupErr == nil && found && identity.matches(operationID, username, mobile) {
			return identity, nil
		}
		return RegistrationIdentity{}, errors.New("Keycloak user creation is unavailable")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	if response.StatusCode == http.StatusConflict {
		identity, found, err := p.FindMobileUserByRegistrationOperation(ctx, operationID, legacyIntent)
		if err != nil {
			return RegistrationIdentity{}, err
		}
		if found {
			if !identity.matches(operationID, username, mobile) {
				return RegistrationIdentity{}, ErrReferenceConflict
			}
			return identity, nil
		}
		return RegistrationIdentity{}, ErrUsernameAlreadyRegistered
	}
	if response.StatusCode != http.StatusCreated {
		return RegistrationIdentity{}, fmt.Errorf("create Keycloak user returned HTTP %d", response.StatusCode)
	}
	location := response.Header.Get("Location")
	subject := location[strings.LastIndex(location, "/")+1:]
	if subject == "" || len(subject) > 128 {
		return RegistrationIdentity{}, errors.New("Keycloak user creation omitted the user identifier")
	}
	identity, found, err := p.FindMobileUserByRegistrationOperation(ctx, operationID, legacyIntent)
	if err != nil || !found || identity.Subject != subject || !identity.matches(operationID, username, mobile) {
		return RegistrationIdentity{}, errors.New("Keycloak registration identity could not be verified")
	}
	identity.PasswordPreserved = false
	return identity, nil
}

func (p *KeycloakDirectory) FindMobileUserByRegistrationOperation(ctx context.Context, operationID uuid.UUID, legacyIntent bool) (RegistrationIdentity, bool, error) {
	if operationID == uuid.Nil {
		return RegistrationIdentity{}, false, ErrReferenceInvalid
	}
	access, err := p.accessToken(ctx)
	if err != nil {
		return RegistrationIdentity{}, false, err
	}
	endpoint := *p.adminURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/admin/realms/" + url.PathEscape(p.realm) + "/users"
	query := endpoint.Query()
	query.Set("q", registrationMarker(legacyIntent)+":"+operationID.String())
	query.Set("max", "2")
	endpoint.RawQuery = query.Encode()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	request.Header.Set("Authorization", "Bearer "+access)
	response, err := p.client.Do(request)
	if err != nil {
		return RegistrationIdentity{}, false, errors.New("Keycloak registration lookup is unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return RegistrationIdentity{}, false, fmt.Errorf("query Keycloak registration correlation returned HTTP %d", response.StatusCode)
	}
	var users []struct {
		ID         string              `json:"id"`
		Username   string              `json:"username"`
		Enabled    bool                `json:"enabled"`
		Attributes map[string][]string `json:"attributes"`
	}
	if err := decodeIdentityJSON(response.Body, 256<<10, &users); err != nil || users == nil {
		return RegistrationIdentity{}, false, errors.New("Keycloak registration lookup failed")
	}
	if len(users) == 0 {
		return RegistrationIdentity{}, false, nil
	}
	if len(users) != 1 {
		return RegistrationIdentity{}, false, ErrReferenceConflict
	}
	user := users[0]
	marker, mobile, verified := user.Attributes[registrationMarker(legacyIntent)], user.Attributes["phone_number"], user.Attributes["phone_number_verified"]
	if strings.TrimSpace(user.ID) == "" || len(user.ID) > 128 || !user.Enabled || strings.TrimSpace(user.Username) == "" || len(marker) != 1 || marker[0] != operationID.String() || len(mobile) != 1 || strings.TrimSpace(mobile[0]) == "" || len(verified) != 1 || verified[0] != "true" {
		return RegistrationIdentity{}, false, ErrReferenceConflict
	}
	return RegistrationIdentity{OperationID: operationID, Subject: user.ID, Username: user.Username, Mobile: mobile[0], PasswordPreserved: true}, true, nil
}

func registrationMarker(legacyIntent bool) string {
	if legacyIntent {
		return "pinvou_registration_id"
	}
	return "pinvou_registration_operation_id"
}

func decodeIdentityJSON(reader io.Reader, max int64, output any) error {
	data, err := io.ReadAll(io.LimitReader(reader, max+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > max {
		return errors.New("Identity response exceeds the size limit")
	}
	return json.Unmarshal(data, output)
}

func (p *KeycloakDirectory) DeleteUser(ctx context.Context, subject string) error {
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

func (p *KeycloakDirectory) DisableUserAndConfirm(ctx context.Context, subject string) error {
	access, err := p.accessToken(ctx)
	if err != nil {
		return err
	}
	endpoint := *p.adminURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/admin/realms/" + url.PathEscape(p.realm) + "/users/" + url.PathEscape(subject)
	body, _ := json.Marshal(map[string]bool{"enabled": false})
	request, _ := http.NewRequestWithContext(ctx, http.MethodPut, endpoint.String(), bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+access)
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil
	}
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("disable Keycloak user returned HTTP %d", response.StatusCode)
	}
	logout := endpoint
	logout.Path += "/logout"
	request, _ = http.NewRequestWithContext(ctx, http.MethodPost, logout.String(), nil)
	request.Header.Set("Authorization", "Bearer "+access)
	response, err = p.client.Do(request)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusNotFound {
		return fmt.Errorf("revoke Keycloak user sessions returned HTTP %d", response.StatusCode)
	}
	request, _ = http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	request.Header.Set("Authorization", "Bearer "+access)
	response, err = p.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil
	}
	var user struct {
		Enabled *bool `json:"enabled"`
	}
	if response.StatusCode != http.StatusOK || decodeIdentityJSON(response.Body, 64<<10, &user) != nil || user.Enabled == nil {
		return errors.New("confirm disabled Keycloak user failed")
	}
	if *user.Enabled {
		return errors.New("Keycloak user remains enabled after compensation")
	}
	return nil
}

func (p *KeycloakDirectory) UpdateUsername(ctx context.Context, subject string, operationID uuid.UUID, username string) error {
	return p.updateUser(ctx, subject, func(user map[string]any) {
		user["username"] = username
		setStringAttribute(user, "pinvou_profile_change_operation_id", operationID.String())
	})
}

func (p *KeycloakDirectory) UpdateMobile(ctx context.Context, subject string, operationID uuid.UUID, mobile string) error {
	return p.updateUser(ctx, subject, func(user map[string]any) {
		setStringAttribute(user, "phone_number", mobile)
		setStringAttribute(user, "phone_number_verified", "true")
		setStringAttribute(user, "pinvou_profile_change_operation_id", operationID.String())
	})
}

func (p *KeycloakDirectory) SetPassword(ctx context.Context, subject, password string) error {
	access, err := p.accessToken(ctx)
	if err != nil {
		return err
	}
	endpoint := p.userEndpoint(subject)
	endpoint.Path += "/reset-password"
	body, _ := json.Marshal(map[string]any{"type": "password", "value": password, "temporary": false})
	request, _ := http.NewRequestWithContext(ctx, http.MethodPut, endpoint.String(), bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+access)
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return fmt.Errorf("reset Keycloak password: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("reset Keycloak password returned HTTP %d", response.StatusCode)
	}
	return nil
}

func (p *KeycloakDirectory) RevokeSessions(ctx context.Context, subject string) error {
	access, err := p.accessToken(ctx)
	if err != nil {
		return err
	}
	endpoint := p.userEndpoint(subject)
	endpoint.Path += "/logout"
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), nil)
	request.Header.Set("Authorization", "Bearer "+access)
	response, err := p.client.Do(request)
	if err != nil {
		return fmt.Errorf("revoke Keycloak sessions: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusNotFound {
		return fmt.Errorf("revoke Keycloak sessions returned HTTP %d", response.StatusCode)
	}
	return nil
}

func (p *KeycloakDirectory) userEndpoint(subject string) url.URL {
	endpoint := *p.adminURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/admin/realms/" + url.PathEscape(p.realm) + "/users/" + url.PathEscape(subject)
	return endpoint
}

func (p *KeycloakDirectory) updateUser(ctx context.Context, subject string, mutate func(map[string]any)) error {
	access, err := p.accessToken(ctx)
	if err != nil {
		return err
	}
	endpoint := p.userEndpoint(subject)
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	request.Header.Set("Authorization", "Bearer "+access)
	response, err := p.client.Do(request)
	if err != nil {
		return err
	}
	var user map[string]any
	if response.StatusCode != http.StatusOK || decodeIdentityJSON(response.Body, 256<<10, &user) != nil || user == nil {
		response.Body.Close()
		return fmt.Errorf("read Keycloak user returned HTTP %d", response.StatusCode)
	}
	response.Body.Close()
	mutate(user)
	body, _ := json.Marshal(user)
	request, _ = http.NewRequestWithContext(ctx, http.MethodPut, endpoint.String(), bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+access)
	request.Header.Set("Content-Type", "application/json")
	response, err = p.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode == http.StatusConflict {
		return ErrUsernameAlreadyRegistered
	}
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("update Keycloak user returned HTTP %d", response.StatusCode)
	}
	return nil
}

func setStringAttribute(user map[string]any, key, value string) {
	attributes, _ := user["attributes"].(map[string]any)
	if attributes == nil {
		attributes = map[string]any{}
		user["attributes"] = attributes
	}
	attributes[key] = []string{value}
}

func (p *KeycloakDirectory) accessToken(ctx context.Context) (string, error) {
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
	if response.StatusCode != http.StatusOK || decodeIdentityJSON(response.Body, 256<<10, &body) != nil || body.AccessToken == "" || body.ExpiresIn <= 0 || body.ExpiresIn > int64((time.Duration(1<<63-1))/time.Second) {
		return "", errors.New("Keycloak provisioner authentication failed")
	}
	p.token = body.AccessToken
	p.expiresAt = time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)
	return p.token, nil
}
