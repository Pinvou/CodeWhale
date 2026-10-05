package authorization

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

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
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
	Allowed bool      `json:"allowed"`
	UserID  uuid.UUID `json:"user_id"`
}

func NewClient(rawURL, serviceToken string) (*Client, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("ACCOUNT_HTTP_URL must be an absolute HTTP(S) URL without credentials, query or fragment")
	}
	serviceToken = strings.TrimSpace(serviceToken)
	if serviceToken == "" {
		return nil, errors.New("INTERNAL_SERVICE_TOKEN is required")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/internal/v1/authorization/check"
	client := observability.NewHTTPClient(3 * time.Second)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{endpoint: parsed, serviceToken: serviceToken, httpClient: client}, nil
}

func (c *Client) Allowed(ctx context.Context, principal identity.Principal, permission string) (bool, http.Header, error) {
	result, headers, err := c.decision(ctx, principal, permission)
	return result.Allowed, headers, err
}

func (c *Client) AuthorizedUserID(ctx context.Context, principal identity.Principal, permission string) (uuid.UUID, error) {
	result, _, err := c.decision(ctx, principal, permission)
	if err != nil {
		return uuid.Nil, err
	}
	if !result.Allowed || result.UserID == uuid.Nil {
		return uuid.Nil, ErrPermissionDenied
	}
	return result.UserID, nil
}

func (c *Client) decision(ctx context.Context, principal identity.Principal, permission string) (decision, http.Header, error) {
	payload, err := json.Marshal(map[string]string{
		"issuer": principal.Issuer, "subject": principal.Subject, "permission": strings.TrimSpace(permission),
	})
	if err != nil {
		return decision{}, nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return decision{}, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.serviceToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return decision{}, nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return decision{}, response.Header, errors.New("account authorization check failed")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return decision{}, response.Header, errors.New("account authorization response is unavailable")
	}
	var result struct {
		Allowed *bool     `json:"allowed"`
		UserID  uuid.UUID `json:"user_id"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return decision{}, response.Header, err
	}
	if result.Allowed == nil {
		return decision{}, response.Header, errors.New("account authorization response omitted decision")
	}
	return decision{Allowed: *result.Allowed, UserID: result.UserID}, response.Header, nil
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
