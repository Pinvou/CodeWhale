package token

import (
	"context"

	"encoding/json"
	"errors"

	"io"
	"net/http"
	"net/url"

	"strings"

	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"

	"github.com/google/uuid"
)

type HTTPContextResolver struct {
	baseURL *url.URL
	client  *http.Client
}

func NewHTTPContextResolver(rawURL string) (*HTTPContextResolver, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, errors.New("ACCOUNT_HTTP_URL must be an absolute URL")
	}
	return &HTTPContextResolver{baseURL: parsed, client: observability.NewHTTPClient(5 * time.Second)}, nil
}

func (r *HTTPContextResolver) Resolve(ctx context.Context, authorization string) (AuthorizationContext, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL.String()+"/v1/me/authorization-context", nil)
	if err != nil {
		return AuthorizationContext{}, err
	}
	request.Header.Set("Authorization", authorization)
	response, err := r.client.Do(request)
	if err != nil {
		return AuthorizationContext{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		var problem struct {
			Code string `json:"code"`
		}
		_ = decodeAccountAuthority(response.Body, 64<<10, &problem)
		if problem.Code == "ACCOUNT_PROVISIONING" {
			return AuthorizationContext{}, ErrAccountProvisioning
		}
		if problem.Code == "ACCOUNT_REPAIR_REQUIRED" {
			return AuthorizationContext{}, ErrAccountRepairRequired
		}
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			return AuthorizationContext{}, ErrForbidden
		}
		return AuthorizationContext{}, ErrAccountAuthorityUnavailable
	}
	var body struct {
		UserID                        uuid.UUID   `json:"user_id"`
		SessionID                     string      `json:"session_id"`
		ContextType                   string      `json:"context_type"`
		OrganizationID                *uuid.UUID  `json:"organization_id"`
		MembershipVersion             int64       `json:"membership_version"`
		OrganizationMembershipVersion *int64      `json:"organization_membership_version"`
		ContextVersion                int64       `json:"context_version"`
		AuthorizationVersion          int64       `json:"authorization_version"`
		UnitIDs                       []uuid.UUID `json:"unit_ids"`
		AncestorUnitIDs               []uuid.UUID `json:"ancestor_unit_ids"`
		Roles                         []string    `json:"roles"`
		Permissions                   []string    `json:"permissions"`
		PersonalRoles                 []string    `json:"personal_roles"`
		PersonalPermissions           []string    `json:"personal_permissions"`
		Mobile                        *string     `json:"mobile"`
		MobileVerified                bool        `json:"mobile_verified"`
	}
	if err := decodeAccountAuthority(response.Body, 1<<20, &body); err != nil {
		return AuthorizationContext{}, err
	}
	if body.UserID == uuid.Nil || body.ContextType != "PERSONAL" || body.OrganizationID != nil || body.ContextVersion < 1 || body.AuthorizationVersion < 1 {
		return AuthorizationContext{}, ErrForbidden
	}
	return AuthorizationContext{UserID: body.UserID, SessionID: body.SessionID, ContextType: body.ContextType, OrganizationID: body.OrganizationID, ContextVersion: body.ContextVersion, AuthorizationVersion: body.AuthorizationVersion, Roles: body.PersonalRoles, Permissions: body.PersonalPermissions}, nil
}

// Retain the reviewed A11 authority boundary: the complete body is bounded
// before decoding, so a valid prefix cannot conceal a second value or overflow.
func decodeAccountAuthority(reader io.Reader, maximum int64, target any) error {
	payload, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return err
	}
	if int64(len(payload)) > maximum {
		return errors.New("Account authority response exceeds its bound")
	}
	return json.Unmarshal(payload, target)
}
