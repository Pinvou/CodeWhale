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
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/google/uuid"
)

var ErrIdentityRegistrationNotFound = errors.New("identity registration correlation was not found")
var ErrIdentityCreateNotDispatched = errors.New("identity creation was authoritatively fenced before dispatch")

type IdentityProvisioner interface {
	ValidateMobileUser(context.Context, uuid.UUID, string, string, string, string) error
	CreateMobileUser(context.Context, uuid.UUID, string, string, string, string, bool, bool) (RegistrationIdentity, error)
	FindMobileUserByRegistrationOperation(context.Context, uuid.UUID, string, string, bool) (RegistrationIdentity, bool, error)
	DisableUserAndConfirm(context.Context, string) error
	DeleteUser(context.Context, string) error
	UpdateUsername(context.Context, string, uuid.UUID, string) error
	UpdateMobile(context.Context, string, uuid.UUID, string) error
	ResetPassword(context.Context, string, uuid.UUID, string, string, string) error
	RevokeSessions(context.Context, string, uuid.UUID, bool) error
}

type RegistrationIdentity struct {
	OperationID       uuid.UUID `json:"registration_operation_id"`
	Subject           string    `json:"subject"`
	Username          string    `json:"username"`
	Mobile            string    `json:"mobile"`
	PasswordPreserved bool      `json:"password_preserved"`
}

type registrationIdentityWire struct {
	OperationID       uuid.UUID `json:"registration_operation_id"`
	Subject           string    `json:"subject"`
	Username          string    `json:"username"`
	Mobile            string    `json:"mobile"`
	PasswordPreserved *bool     `json:"password_preserved"`
}

func (wire registrationIdentityWire) bound(operationID uuid.UUID, username, mobile string) (RegistrationIdentity, error) {
	if operationID == uuid.Nil || wire.OperationID != operationID || wire.Username != username || wire.Mobile != mobile || strings.TrimSpace(wire.Subject) == "" || wire.PasswordPreserved == nil {
		return RegistrationIdentity{}, errors.New("identity adapter returned inconsistent registration facts")
	}
	return RegistrationIdentity{OperationID: wire.OperationID, Subject: wire.Subject, Username: wire.Username, Mobile: wire.Mobile, PasswordPreserved: *wire.PasswordPreserved}, nil
}

func (p *HTTPIdentityProvisioner) UpdateUsername(ctx context.Context, subject string, operationID uuid.UUID, username string) error {
	return p.request(ctx, http.MethodPut, "/internal/v1/users/"+url.PathEscape(subject)+"/username", map[string]any{"operation_id": operationID, "username": username}, nil)
}

func (p *HTTPIdentityProvisioner) UpdateMobile(ctx context.Context, subject string, operationID uuid.UUID, mobile string) error {
	return p.request(ctx, http.MethodPut, "/internal/v1/users/"+url.PathEscape(subject)+"/mobile", map[string]any{"operation_id": operationID, "mobile": mobile}, nil)
}

func (p *HTTPIdentityProvisioner) ResetPassword(ctx context.Context, subject string, operationID uuid.UUID, mobile, handle, policyVersion string) error {
	return p.request(ctx, http.MethodPost, "/internal/v1/users/"+url.PathEscape(subject)+"/password-resets", map[string]any{"operation_id": operationID, "mobile": mobile, "password_handle": handle, "password_policy_version": policyVersion}, nil)
}

func (p *HTTPIdentityProvisioner) RevokeSessions(ctx context.Context, subject string, operationID uuid.UUID, firstAttempt bool) error {
	return p.request(ctx, http.MethodPost, "/internal/v1/users/"+url.PathEscape(subject)+"/revoke-sessions", map[string]any{"operation_id": operationID, "first_attempt": firstAttempt}, nil)
}

type HTTPIdentityProvisioner struct {
	baseURL      *url.URL
	serviceToken string
	client       *http.Client
}

func NewHTTPIdentityProvisioner(rawURL, serviceToken string) (*HTTPIdentityProvisioner, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("IDENTITY_HTTP_URL must be an absolute URL without credentials")
	}
	if strings.TrimSpace(serviceToken) == "" {
		return nil, errors.New("INTERNAL_SERVICE_TOKEN is required")
	}
	client := observability.NewHTTPClient(10 * time.Second)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &HTTPIdentityProvisioner{baseURL: parsed, serviceToken: strings.TrimSpace(serviceToken), client: client}, nil
}

func (p *HTTPIdentityProvisioner) ValidateMobileUser(ctx context.Context, operationID uuid.UUID, username, mobile, handle, policyVersion string) error {
	return p.request(ctx, http.MethodPost, "/internal/v1/registration/password-references/validate", map[string]string{
		"registration_operation_id": operationID.String(), "username": username, "mobile": mobile, "password_handle": handle, "password_policy_version": policyVersion,
	}, nil)
}

func (p *HTTPIdentityProvisioner) CreateMobileUser(ctx context.Context, operationID uuid.UUID, username, mobile, handle, policyVersion string, legacyIntent, firstCreateAttempt bool) (RegistrationIdentity, error) {
	var response registrationIdentityWire
	err := p.request(ctx, http.MethodPost, "/internal/v1/users/mobile", map[string]any{
		"registration_operation_id": operationID.String(), "username": username, "mobile": mobile, "password_handle": handle, "password_policy_version": policyVersion,
		"legacy_intent": legacyIntent, "first_create_attempt": firstCreateAttempt,
	}, &response)
	if err != nil {
		return RegistrationIdentity{}, err
	}
	return response.bound(operationID, username, mobile)
}

func (p *HTTPIdentityProvisioner) FindMobileUserByRegistrationOperation(ctx context.Context, operationID uuid.UUID, username, mobile string, legacyIntent bool) (RegistrationIdentity, bool, error) {
	var response registrationIdentityWire
	path := "/internal/v1/registration-operations/" + url.PathEscape(operationID.String()) + "/user"
	if legacyIntent {
		path += "?legacy_intent=true"
	}
	err := p.request(ctx, http.MethodGet, path, nil, &response)
	if errors.Is(err, ErrIdentityRegistrationNotFound) {
		return RegistrationIdentity{}, false, nil
	}
	if err != nil {
		return RegistrationIdentity{}, false, err
	}
	identity, err := response.bound(operationID, username, mobile)
	if err != nil || !identity.PasswordPreserved {
		return RegistrationIdentity{}, false, errors.New("identity adapter returned inconsistent recovery facts")
	}
	return identity, true, nil
}

func (p *HTTPIdentityProvisioner) DeleteUser(ctx context.Context, subject string) error {
	return p.request(ctx, http.MethodDelete, "/internal/v1/users/"+url.PathEscape(subject), nil, nil)
}

func (p *HTTPIdentityProvisioner) DisableUserAndConfirm(ctx context.Context, subject string) error {
	return p.request(ctx, http.MethodPost, "/internal/v1/users/"+url.PathEscape(subject)+"/disable-and-confirm", nil, nil)
}

func (p *HTTPIdentityProvisioner) request(ctx context.Context, method, path string, body any, output any) error {
	endpoint := *p.baseURL
	relative, err := url.Parse(path)
	if err != nil {
		return errors.New("invalid identity adapter request")
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + relative.Path
	endpoint.RawQuery = relative.RawQuery
	var reader io.Reader
	if body != nil {
		encoded, _ := json.Marshal(body)
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), reader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+p.serviceToken)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := p.client.Do(request)
	if err != nil {
		return errors.New("identity adapter request failed")
	}
	defer response.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if readErr != nil || len(data) > 64<<10 {
		return errors.New("identity adapter returned an invalid response")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == http.StatusNotFound {
			return ErrIdentityRegistrationNotFound
		}
		var problem struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(data, &problem)
		switch problem.Code {
		case "IDENTITY_CREATE_NOT_DISPATCHED":
			if response.StatusCode == http.StatusGone {
				return ErrIdentityCreateNotDispatched
			}
			return errors.New("identity adapter returned an invalid termination proof")
		case "PASSWORD_REFERENCE_INVALID":
			return ErrPasswordReferenceInvalid
		case "PASSWORD_REFERENCE_CONFLICT":
			return ErrPasswordReferenceConflict
		case "USERNAME_ALREADY_REGISTERED":
			return ErrUsernameAlreadyRegistered
		default:
			return fmt.Errorf("identity adapter returned HTTP %d", response.StatusCode)
		}
	}
	expectedStatus := http.StatusNoContent
	if output != nil {
		expectedStatus = http.StatusOK
		if method == http.MethodPost {
			expectedStatus = http.StatusCreated
		}
	}
	if response.StatusCode != expectedStatus || (output == nil && len(data) != 0) {
		return errors.New("identity adapter returned an invalid success response")
	}
	if output != nil {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if decoder.Decode(output) != nil || decoder.Decode(new(any)) != io.EOF {
			return errors.New("identity adapter returned an invalid response")
		}
	}
	return nil
}
