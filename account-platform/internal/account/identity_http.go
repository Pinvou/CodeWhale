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
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

var ErrIdentityRegistrationNotFound = errors.New("identity registration correlation was not found")

type IdentityProvisioner interface {
	ValidateMobileUser(context.Context, uuid.UUID, string, string, string, string) error
	CreateMobileUser(context.Context, uuid.UUID, string, string, string, string) (string, error)
	FindMobileUserByRegistrationOperation(context.Context, uuid.UUID) (string, bool, error)
	DisableUserAndConfirm(context.Context, string) error
	UpdateUsername(context.Context, string, uuid.UUID, int64, string) error
	UpdateMobile(context.Context, string, uuid.UUID, int64, string) error
	ResetPassword(context.Context, string, uuid.UUID, int64, string, string, string) error
	RevokeSessions(context.Context, string) error
}

func (p *HTTPIdentityProvisioner) UpdateUsername(ctx context.Context, subject string, operationID uuid.UUID, fenceVersion int64, username string) error {
	return p.request(ctx, http.MethodPut, "/internal/v1/users/"+url.PathEscape(subject)+"/username", map[string]any{"operation_id": operationID, "fence_version": fenceVersion, "username": username}, nil)
}

func (p *HTTPIdentityProvisioner) UpdateMobile(ctx context.Context, subject string, operationID uuid.UUID, fenceVersion int64, mobile string) error {
	return p.request(ctx, http.MethodPut, "/internal/v1/users/"+url.PathEscape(subject)+"/mobile", map[string]any{"operation_id": operationID, "fence_version": fenceVersion, "mobile": mobile}, nil)
}

func (p *HTTPIdentityProvisioner) ResetPassword(ctx context.Context, subject string, operationID uuid.UUID, fenceVersion int64, mobile, handle, policyVersion string) error {
	return p.request(ctx, http.MethodPost, "/internal/v1/users/"+url.PathEscape(subject)+"/password-resets", map[string]any{"operation_id": operationID, "fence_version": fenceVersion, "mobile": mobile, "password_handle": handle, "password_policy_version": policyVersion}, nil)
}

func (p *HTTPIdentityProvisioner) RevokeSessions(ctx context.Context, subject string) error {
	return p.request(ctx, http.MethodPost, "/internal/v1/users/"+url.PathEscape(subject)+"/revoke-sessions", nil, nil)
}

type HTTPIdentityProvisioner struct {
	baseURL      *url.URL
	serviceToken string
	client       *http.Client
}

func NewHTTPIdentityProvisioner(rawURL, serviceToken string) (*HTTPIdentityProvisioner, error) {
	parsed, err := authorityEndpoint(rawURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(serviceToken) == "" {
		return nil, errors.New("INTERNAL_SERVICE_TOKEN is required")
	}
	return &HTTPIdentityProvisioner{baseURL: parsed, serviceToken: strings.TrimSpace(serviceToken), client: observability.NewHTTPClient(10 * time.Second)}, nil
}

func (p *HTTPIdentityProvisioner) ValidateMobileUser(ctx context.Context, operationID uuid.UUID, username, mobile, handle, policyVersion string) error {
	return p.request(ctx, http.MethodPost, "/internal/v1/registration/password-references/validate", map[string]string{
		"registration_operation_id": operationID.String(), "username": username, "mobile": mobile, "password_handle": handle, "password_policy_version": policyVersion,
	}, nil)
}

func (p *HTTPIdentityProvisioner) CreateMobileUser(ctx context.Context, operationID uuid.UUID, username, mobile, handle, policyVersion string) (string, error) {
	var response struct {
		Subject string `json:"subject"`
	}
	err := p.request(ctx, http.MethodPost, "/internal/v1/users/mobile", map[string]string{
		"registration_operation_id": operationID.String(), "username": username, "mobile": mobile, "password_handle": handle, "password_policy_version": policyVersion,
	}, &response)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(response.Subject) == "" {
		return "", errors.New("identity adapter omitted the subject")
	}
	return response.Subject, nil
}

func (p *HTTPIdentityProvisioner) FindMobileUserByRegistrationOperation(ctx context.Context, operationID uuid.UUID) (string, bool, error) {
	var response struct {
		Subject string `json:"subject"`
	}
	err := p.request(ctx, http.MethodGet, "/internal/v1/registration-operations/"+url.PathEscape(operationID.String())+"/user", nil, &response)
	if errors.Is(err, ErrIdentityRegistrationNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if strings.TrimSpace(response.Subject) == "" {
		return "", false, errors.New("identity adapter omitted the subject")
	}
	return response.Subject, true, nil
}

func (p *HTTPIdentityProvisioner) DisableUserAndConfirm(ctx context.Context, subject string) error {
	return p.request(ctx, http.MethodPost, "/internal/v1/users/"+url.PathEscape(subject)+"/disable-and-confirm", nil, nil)
}

func (p *HTTPIdentityProvisioner) request(ctx context.Context, method, path string, body any, output any) error {
	endpoint := *p.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + path
	var reader io.Reader
	if body != nil {
		encoded, _ := json.Marshal(body)
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), reader)
	if err != nil {
		return err
	}
	if err := serviceauth.SetAuthorization(request, p.serviceToken); err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := p.client.Do(request)
	if err != nil {
		return errors.New("identity adapter request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == http.StatusNotFound {
			return ErrIdentityRegistrationNotFound
		}
		var problem struct {
			Code string `json:"code"`
		}
		_ = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&problem)
		switch problem.Code {
		case "PASSWORD_INVALID":
			return ErrPasswordInvalid
		case "PASSWORD_EDGE_SPACE_NOT_ALLOWED":
			return ErrPasswordEdgeSpace
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
	// Mutation endpoints acknowledge completed work with 204. An accepted or
	// otherwise successful response must not advance the caller's durable saga.
	if output == nil && response.StatusCode != http.StatusNoContent {
		return errors.New("identity adapter did not confirm operation completion")
	}
	if output != nil && decodeAuthorityResponse(response.Body, output) != nil {
		return errors.New("identity adapter returned an invalid response")
	}
	return nil
}
