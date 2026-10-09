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
	if response.StatusCode != http.StatusOK || decodeCompleteKeycloakJSON(response.Body, 128<<10, &users) != nil {
		response.Body.Close()
		return "", fmt.Errorf("query bootstrap Keycloak user returned HTTP %d", response.StatusCode)
	}
	response.Body.Close()
	if len(users) == 1 && strings.EqualFold(users[0].Username, username) {
		if strings.TrimSpace(users[0].ID) == "" {
			return "", errors.New("bootstrap Keycloak user has no identity")
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

func NewKeycloakDirectory(adminURL, tokenURL, realm, clientID, clientSecret string) (*KeycloakDirectory, error) {
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
	return &KeycloakDirectory{adminURL: admin, tokenURL: token, realm: strings.TrimSpace(realm), clientID: strings.TrimSpace(clientID), clientSecret: strings.TrimSpace(clientSecret), client: observability.NewHTTPClient(10 * time.Second)}, nil
}

func (p *KeycloakDirectory) Ping(ctx context.Context) error {
	_, err := p.accessToken(ctx)
	return err
}

func (p *KeycloakDirectory) CreateMobileUser(ctx context.Context, operationID uuid.UUID, username, mobile, password string) (string, error) {
	access, err := p.accessToken(ctx)
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]any{
		"username": username, "enabled": true,
		"attributes": map[string][]string{
			"phone_number": {mobile}, "phone_number_verified": {"true"},
			"pinvou_registration_operation_id": {operationID.String()},
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
		return "", fmt.Errorf("create Keycloak user: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	if response.StatusCode == http.StatusConflict {
		return "", ErrUsernameAlreadyRegistered
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

func (p *KeycloakDirectory) FindMobileUserByRegistrationOperation(ctx context.Context, operationID uuid.UUID) (string, bool, error) {
	access, err := p.accessToken(ctx)
	if err != nil {
		return "", false, err
	}
	endpoint := *p.adminURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/admin/realms/" + url.PathEscape(p.realm) + "/users"
	query := endpoint.Query()
	query.Set("q", "pinvou_registration_operation_id:"+operationID.String())
	query.Set("max", "2")
	endpoint.RawQuery = query.Encode()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	request.Header.Set("Authorization", "Bearer "+access)
	response, err := p.client.Do(request)
	if err != nil {
		return "", false, fmt.Errorf("query Keycloak registration correlation: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("query Keycloak registration correlation returned HTTP %d", response.StatusCode)
	}
	var users []struct {
		ID         string              `json:"id"`
		Attributes map[string][]string `json:"attributes"`
	}
	if err := decodeCompleteKeycloakJSON(response.Body, 256<<10, &users); err != nil {
		return "", false, err
	}
	matched := ""
	for _, user := range users {
		values := user.Attributes["pinvou_registration_operation_id"]
		if len(values) == 1 && values[0] == operationID.String() {
			if strings.TrimSpace(user.ID) == "" {
				return "", false, errors.New("correlated Keycloak user has no identity")
			}
			if matched != "" && matched != user.ID {
				return "", false, errors.New("multiple Keycloak users share a registration correlation")
			}
			matched = user.ID
		}
	}
	return matched, matched != "", nil
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
	if response.StatusCode != http.StatusOK {
		return errors.New("confirm disabled Keycloak user failed")
	}
	if decodeCompleteKeycloakJSON(response.Body, 64<<10, &user) != nil || user.Enabled == nil {
		return errors.New("confirm disabled Keycloak user failed")
	}
	if *user.Enabled {
		return errors.New("Keycloak user remains enabled after compensation")
	}
	return nil
}

func (p *KeycloakDirectory) UpdateUsername(ctx context.Context, subject string, operationID uuid.UUID, fenceVersion int64, username string) error {
	return p.atomicProfileChange(ctx, subject, operationID, fenceVersion, "USERNAME", username)
}

func (p *KeycloakDirectory) UpdateMobile(ctx context.Context, subject string, operationID uuid.UUID, fenceVersion int64, mobile string) error {
	return p.atomicProfileChange(ctx, subject, operationID, fenceVersion, "MOBILE", mobile)
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

// SetPasswordForOperation makes a password mutation repeatable across Account
// worker retries.  The marker is stored with the identity, so a restarted
// Identity Adapter can prove that the password step already completed without
// retaining or asking Account to persist the cleartext password.
func (p *KeycloakDirectory) SetPasswordForOperation(ctx context.Context, subject string, operationID uuid.UUID, fenceVersion int64, password string) error {
	_, err := p.atomicPasswordReset(ctx, subject, operationID, fenceVersion, &password)
	return err
}

func (p *KeycloakDirectory) PasswordResetApplied(ctx context.Context, subject string, operationID uuid.UUID, fenceVersion int64) (bool, error) {
	return p.atomicPasswordReset(ctx, subject, operationID, fenceVersion, nil)
}

func (p *KeycloakDirectory) atomicPasswordReset(ctx context.Context, subject string, operationID uuid.UUID, fenceVersion int64, password *string) (bool, error) {
	access, err := p.accessToken(ctx)
	if err != nil {
		return false, err
	}
	endpoint := *p.adminURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/realms/" + url.PathEscape(p.realm) + "/pinvou-identity-security/users/" + url.PathEscape(subject) + "/password-reset"
	body, _ := json.Marshal(map[string]any{"operationId": operationID, "fenceVersion": fenceVersion, "password": password})
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+access)
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return false, fmt.Errorf("atomic Keycloak password reset: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		return true, nil
	}
	var problem struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&problem)
	switch problem.Code {
	case "PASSWORD_RESET_NOT_APPLIED":
		if response.StatusCode == http.StatusNotFound && password == nil {
			return false, nil
		}
		return false, fmt.Errorf("atomic Keycloak password reset returned HTTP %d", response.StatusCode)
	case "PASSWORD_REFERENCE_CONFLICT":
		return false, ErrReferenceConflict
	case "PASSWORD_INVALID":
		return false, ErrPasswordInvalid
	default:
		return false, fmt.Errorf("atomic Keycloak password reset returned HTTP %d", response.StatusCode)
	}
}

func (p *KeycloakDirectory) atomicProfileChange(ctx context.Context, subject string, operationID uuid.UUID, fenceVersion int64, field, value string) error {
	access, err := p.accessToken(ctx)
	if err != nil {
		return err
	}
	endpoint := *p.adminURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/realms/" + url.PathEscape(p.realm) + "/pinvou-identity-security/users/" + url.PathEscape(subject) + "/profile-change"
	body, _ := json.Marshal(map[string]any{"operationId": operationID, "fenceVersion": fenceVersion, "field": field, "value": value})
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+access)
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return fmt.Errorf("atomic Keycloak profile change: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		return nil
	}
	var problem struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&problem)
	switch problem.Code {
	case "IDENTITY_SECURITY_FENCE_CONFLICT":
		return ErrReferenceConflict
	case "USERNAME_ALREADY_REGISTERED":
		return ErrUsernameAlreadyRegistered
	default:
		return fmt.Errorf("atomic Keycloak profile change returned HTTP %d", response.StatusCode)
	}
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
	user, err := p.readUser(ctx, subject)
	if err != nil {
		return err
	}
	mutate(user)
	return p.putUser(ctx, subject, user)
}

func (p *KeycloakDirectory) putUser(ctx context.Context, subject string, user map[string]any) error {
	access, err := p.accessToken(ctx)
	if err != nil {
		return err
	}
	endpoint := p.userEndpoint(subject)
	body, _ := json.Marshal(user)
	request, _ := http.NewRequestWithContext(ctx, http.MethodPut, endpoint.String(), bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+access)
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
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

func (p *KeycloakDirectory) readUser(ctx context.Context, subject string) (map[string]any, error) {
	access, err := p.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	endpoint := p.userEndpoint(subject)
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	request.Header.Set("Authorization", "Bearer "+access)
	response, err := p.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var user map[string]any
	if response.StatusCode != http.StatusOK || decodeCompleteKeycloakJSON(response.Body, 256<<10, &user) != nil {
		return nil, fmt.Errorf("read Keycloak user returned HTTP %d", response.StatusCode)
	}
	return user, nil
}

// Keycloak authority reads must distinguish an explicit object/array from
// null, truncated input or a second value before callers mutate identity state.
func decodeCompleteKeycloakJSON(reader io.Reader, maximum int64, target any) error {
	body, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return err
	}
	if int64(len(body)) > maximum || bytes.Equal(bytes.TrimSpace(body), []byte("null")) {
		return errors.New("Keycloak authority response is incomplete or too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("Keycloak authority response must contain one complete JSON value")
	}
	return nil
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
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 256<<10)).Decode(&body) != nil || body.AccessToken == "" {
		return "", errors.New("Keycloak provisioner authentication failed")
	}
	p.token = body.AccessToken
	p.expiresAt = time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)
	return p.token, nil
}
