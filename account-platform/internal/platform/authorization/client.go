package authorization

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type Client struct {
	endpoint     *url.URL
	serviceToken string
	httpClient   *http.Client
}

var ErrPermissionDenied = errors.New("permission is not granted")

type decision struct {
	Allowed            bool      `json:"allowed"`
	UserID             uuid.UUID `json:"user_id"`
	GrantedPermissions []string  `json:"granted_permissions,omitempty"`
}

func NewClient(rawURL, serviceToken string) (*Client, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, errors.New("ACCOUNT_HTTP_URL must be an absolute URL")
	}
	serviceToken = strings.TrimSpace(serviceToken)
	if serviceToken == "" {
		return nil, errors.New("INTERNAL_SERVICE_TOKEN is required")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/internal/v1/authorization/check"
	return &Client{endpoint: parsed, serviceToken: serviceToken, httpClient: observability.NewHTTPClient(3 * time.Second)}, nil
}

func (c *Client) Allowed(ctx context.Context, principal identity.Principal, permission string) (bool, http.Header, error) {
	result, headers, err := c.decision(ctx, principal, permission, nil)
	return result.Allowed, headers, err
}

func (c *Client) allowedComposite(ctx context.Context, principal identity.Principal, anyPermissions, allPermissions []string) (decision, http.Header, error) {
	payload, err := json.Marshal(map[string]any{
		"issuer": principal.Issuer, "subject": principal.Subject,
		"any_permissions": anyPermissions, "all_permissions": allPermissions,
	})
	if err != nil {
		return decision{}, nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return decision{}, nil, err
	}
	if err := serviceauth.SetAuthorization(request, c.serviceToken); err != nil {
		return decision{}, nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return decision{}, nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return decision{}, response.Header, errors.New("account composite authorization check failed")
	}
	result, err := decodeDecision(response.Body)
	if err != nil {
		return decision{}, response.Header, err
	}
	return result, response.Header, nil
}

func (c *Client) AuthorizedUserID(ctx context.Context, principal identity.Principal, permission string) (uuid.UUID, error) {
	result, _, err := c.decision(ctx, principal, permission, nil)
	if err != nil {
		return uuid.Nil, err
	}
	if !result.Allowed || result.UserID == uuid.Nil {
		return uuid.Nil, ErrPermissionDenied
	}
	return result.UserID, nil
}

// AuthorizedUserIDAll evaluates all permissions in one Account authorization
// snapshot. The returned version must be persisted with governed commands so a
// later authorization change cannot be merged with an earlier decision.
func (c *Client) AuthorizedUserIDAll(ctx context.Context, principal identity.Principal, permissions ...string) (uuid.UUID, int64, error) {
	result, headers, err := c.allowedComposite(ctx, principal, nil, permissions)
	if err != nil {
		return uuid.Nil, 0, err
	}
	version, valid := positiveAuthorizationVersion(headers)
	if !valid {
		return uuid.Nil, 0, errors.New("authorization version is unavailable")
	}
	if !result.Allowed || result.UserID == uuid.Nil {
		return uuid.Nil, 0, ErrPermissionDenied
	}
	return result.UserID, version, nil
}

func (c *Client) decision(ctx context.Context, principal identity.Principal, permission string, organizationID *uuid.UUID) (decision, http.Header, error) {
	payload, err := json.Marshal(map[string]any{
		"issuer": principal.Issuer, "subject": principal.Subject, "permission": strings.TrimSpace(permission), "target_organization_id": organizationID,
	})
	if err != nil {
		return decision{}, nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return decision{}, nil, err
	}
	if err := serviceauth.SetAuthorization(request, c.serviceToken); err != nil {
		return decision{}, nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return decision{}, nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return decision{}, response.Header, errors.New("account authorization check failed")
	}
	result, err := decodeDecision(response.Body)
	if err != nil {
		return decision{}, response.Header, err
	}
	return result, response.Header, nil
}

func (c *Client) Require(permission string) gin.HandlerFunc {
	return func(ginContext *gin.Context) {
		principal, ok := identity.FromContext(ginContext.Request.Context())
		if !ok {
			api.WriteError(ginContext.Writer, http.StatusUnauthorized, "AUTH_REQUIRED", "a valid bearer token is required", httpserver.RequestID(ginContext.Request.Context()))
			ginContext.Abort()
			return
		}
		allowed, headers, err := c.Allowed(ginContext.Request.Context(), principal, permission)
		if err != nil {
			api.WriteError(ginContext.Writer, http.StatusServiceUnavailable, "AUTHORIZATION_UNAVAILABLE", "authorization is temporarily unavailable", httpserver.RequestID(ginContext.Request.Context()))
			ginContext.Abort()
			return
		}
		if !allowed {
			api.WriteError(ginContext.Writer, http.StatusForbidden, "PERMISSION_DENIED", "the requested permission is not granted", httpserver.RequestID(ginContext.Request.Context()))
			ginContext.Abort()
			return
		}
		for _, name := range []string{"X-Context-Version", "X-Authorization-Version"} {
			if value := headers.Get(name); value != "" {
				ginContext.Header(name, value)
			}
		}
		ginContext.Next()
	}
}

type grantedPermissionsContextKey struct{}

// WithGrantedPermissions records the permissions that were verified for the
// current request. It is intentionally request-scoped so handlers can redact
// mixed-domain responses without trusting client-provided selectors.
func WithGrantedPermissions(ctx context.Context, permissions ...string) context.Context {
	granted := make(map[string]struct{}, len(permissions))
	for _, permission := range permissions {
		if permission = strings.TrimSpace(permission); permission != "" {
			granted[permission] = struct{}{}
		}
	}
	return context.WithValue(ctx, grantedPermissionsContextKey{}, granted)
}

func GrantedPermission(ctx context.Context, permission string) bool {
	granted, _ := ctx.Value(grantedPermissionsContextKey{}).(map[string]struct{})
	_, ok := granted[strings.TrimSpace(permission)]
	return ok
}

func positiveAuthorizationVersion(headers http.Header) (int64, bool) {
	value := strings.TrimSpace(headers.Get("X-Authorization-Version"))
	version, err := strconv.ParseInt(value, 10, 64)
	return version, err == nil && version > 0
}

// RequireAnyAnd atomically evaluates (ANY(anyPermissions) AND ALL(allPermissions))
// in one Account request and records exactly the permissions granted by that
// response. No version from another authorization request can be merged.
func (c *Client) RequireAnyAnd(anyPermissions []string, allPermissions []string) gin.HandlerFunc {
	return func(ginContext *gin.Context) {
		principal, ok := identity.FromContext(ginContext.Request.Context())
		if !ok {
			api.WriteError(ginContext.Writer, http.StatusUnauthorized, "AUTH_REQUIRED", "a valid bearer token is required", httpserver.RequestID(ginContext.Request.Context()))
			ginContext.Abort()
			return
		}
		result, headers, err := c.allowedComposite(ginContext.Request.Context(), principal, anyPermissions, allPermissions)
		if err != nil {
			api.WriteError(ginContext.Writer, http.StatusServiceUnavailable, "AUTHORIZATION_UNAVAILABLE", "authorization is temporarily unavailable", httpserver.RequestID(ginContext.Request.Context()))
			ginContext.Abort()
			return
		}
		if _, valid := positiveAuthorizationVersion(headers); !valid {
			api.WriteError(ginContext.Writer, http.StatusServiceUnavailable, "AUTHORIZATION_VERSION_INVALID", "authorization version is unavailable", httpserver.RequestID(ginContext.Request.Context()))
			ginContext.Abort()
			return
		}
		if !result.Allowed {
			api.WriteError(ginContext.Writer, http.StatusForbidden, "PERMISSION_DENIED", "the requested permission expression is not granted", httpserver.RequestID(ginContext.Request.Context()))
			ginContext.Abort()
			return
		}
		for _, name := range []string{"X-Context-Version", "X-Authorization-Version"} {
			if value := headers.Get(name); value != "" {
				ginContext.Header(name, value)
			}
		}
		ginContext.Request = ginContext.Request.WithContext(WithGrantedPermissions(ginContext.Request.Context(), result.GrantedPermissions...))
		ginContext.Next()
	}
}

// RequireAny evaluates all alternatives in one Account authorization snapshot,
// permits the request when at least one is granted, and records verified grants.
func (c *Client) RequireAny(permissions ...string) gin.HandlerFunc {
	return func(ginContext *gin.Context) {
		principal, ok := identity.FromContext(ginContext.Request.Context())
		if !ok {
			api.WriteError(ginContext.Writer, http.StatusUnauthorized, "AUTH_REQUIRED", "a valid bearer token is required", httpserver.RequestID(ginContext.Request.Context()))
			ginContext.Abort()
			return
		}
		result, versionHeaders, err := c.allowedComposite(ginContext.Request.Context(), principal, permissions, nil)
		if err != nil {
			api.WriteError(ginContext.Writer, http.StatusServiceUnavailable, "AUTHORIZATION_UNAVAILABLE", "authorization is temporarily unavailable", httpserver.RequestID(ginContext.Request.Context()))
			ginContext.Abort()
			return
		}
		if _, valid := positiveAuthorizationVersion(versionHeaders); !valid {
			api.WriteError(ginContext.Writer, http.StatusServiceUnavailable, "AUTHORIZATION_VERSION_INVALID", "authorization version is unavailable", httpserver.RequestID(ginContext.Request.Context()))
			ginContext.Abort()
			return
		}
		if !result.Allowed || len(result.GrantedPermissions) == 0 {
			api.WriteError(ginContext.Writer, http.StatusForbidden, "PERMISSION_DENIED", "none of the requested permissions is granted", httpserver.RequestID(ginContext.Request.Context()))
			ginContext.Abort()
			return
		}
		for _, name := range []string{"X-Context-Version", "X-Authorization-Version"} {
			if value := versionHeaders.Get(name); value != "" {
				ginContext.Header(name, value)
			}
		}
		ginContext.Request = ginContext.Request.WithContext(WithGrantedPermissions(ginContext.Request.Context(), result.GrantedPermissions...))
		ginContext.Next()
	}
}

// decodeDecision preserves the reviewed complete, bounded authorization reply
// contract. Missing allowed is not an explicit denial and must fail closed.
func decodeDecision(reader io.Reader) (decision, error) {
	body, err := io.ReadAll(io.LimitReader(reader, (64<<10)+1))
	if err != nil {
		return decision{}, err
	}
	if len(body) > 64<<10 {
		return decision{}, errors.New("account authorization reply is too large")
	}
	var wire struct {
		Allowed            *bool     `json:"allowed"`
		UserID             uuid.UUID `json:"user_id"`
		GrantedPermissions []string  `json:"granted_permissions,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&wire); err != nil {
		return decision{}, err
	}
	if wire.Allowed == nil {
		return decision{}, errors.New("account authorization reply is missing allowed")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return decision{}, errors.New("account authorization reply must contain one complete JSON value")
	}
	return decision{Allowed: *wire.Allowed, UserID: wire.UserID, GrantedPermissions: wire.GrantedPermissions}, nil
}
