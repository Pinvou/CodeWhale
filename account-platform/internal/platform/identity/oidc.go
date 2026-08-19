package identity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gin-gonic/gin"
)

type Principal struct {
	Issuer        string
	Subject       string
	SessionID     string
	Email         string
	DisplayName   string
	Username      string
	PhoneNumber   string
	PhoneVerified bool
	RealmRoles    []string
}

type VerifyFunc func(context.Context, string) (Principal, error)

type principalContextKey struct{}

func FromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(Principal)
	return principal, ok
}

func NewOIDCVerifyFunc(ctx context.Context, issuer, audience, environment string) (VerifyFunc, error) {
	return NewOIDCVerifyFuncWithJWKS(ctx, issuer, "", audience, environment)
}

func NewOIDCVerifyFuncWithJWKS(ctx context.Context, issuer, jwksURL, audience, environment string) (VerifyFunc, error) {
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	jwksURL = strings.TrimSpace(jwksURL)
	audience = strings.TrimSpace(audience)
	parsed, err := url.Parse(issuer)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, errors.New("OIDC_ISSUER must be an absolute HTTP(S) URL")
	}
	if environment != "development" && environment != "test" && parsed.Scheme != "https" {
		return nil, errors.New("OIDC_ISSUER must use HTTPS outside development and test")
	}
	if audience == "" {
		return nil, errors.New("OIDC_AUDIENCE is required")
	}
	if jwksURL == "" {
		jwksURL = issuer + "/protocol/openid-connect/certs"
	}
	parsedJWKS, err := url.Parse(jwksURL)
	if err != nil || parsedJWKS.Host == "" || (parsedJWKS.Scheme != "https" && parsedJWKS.Scheme != "http") {
		return nil, errors.New("OIDC_JWKS_URL must be an absolute HTTP(S) URL")
	}
	if environment != "development" && environment != "test" && parsedJWKS.Scheme != "https" {
		return nil, errors.New("OIDC_JWKS_URL must use HTTPS outside development and test")
	}

	keySet := oidc.NewRemoteKeySet(ctx, jwksURL)
	verifier := oidc.NewVerifier(issuer, keySet, &oidc.Config{ClientID: audience})
	return func(ctx context.Context, raw string) (Principal, error) {
		token, err := verifier.Verify(ctx, raw)
		if err != nil {
			return Principal{}, fmt.Errorf("verify access token: %w", err)
		}
		var claims struct {
			SessionID     string `json:"sid"`
			SessionState  string `json:"session_state"`
			Email         string `json:"email"`
			Name          string `json:"name"`
			PreferredName string `json:"preferred_username"`
			PhoneNumber   string `json:"phone_number"`
			PhoneVerified bool   `json:"phone_number_verified"`
			RealmAccess   struct {
				Roles []string `json:"roles"`
			} `json:"realm_access"`
		}
		if err := token.Claims(&claims); err != nil {
			return Principal{}, fmt.Errorf("decode access token claims: %w", err)
		}
		sessionID := claims.SessionID
		if sessionID == "" {
			sessionID = claims.SessionState
		}
		if token.Subject == "" || sessionID == "" {
			return Principal{}, errors.New("access token is missing subject or session identifier")
		}
		displayName := claims.Name
		if displayName == "" {
			displayName = claims.PreferredName
		}
		return Principal{
			Issuer:        issuer,
			Subject:       token.Subject,
			SessionID:     sessionID,
			Email:         claims.Email,
			DisplayName:   displayName,
			Username:      claims.PreferredName,
			PhoneNumber:   claims.PhoneNumber,
			PhoneVerified: claims.PhoneVerified,
			RealmRoles:    slices.Clone(claims.RealmAccess.Roles),
		}, nil
	}, nil
}

func HasRealmRole(principal Principal, role string) bool {
	return slices.Contains(principal.RealmRoles, strings.TrimSpace(role))
}

func RequireRealmRoleGin(role string) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := FromContext(c.Request.Context())
		if !ok || !HasRealmRole(principal, role) {
			api.WriteError(c.Writer, http.StatusForbidden, "ADMIN_REQUIRED", "platform administrator access is required", httpserver.RequestID(c.Request.Context()))
			c.Abort()
			return
		}
		c.Next()
	}
}

func Middleware(verify VerifyFunc, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		parts := strings.Fields(header)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
			api.WriteError(w, http.StatusUnauthorized, "AUTH_REQUIRED", "a valid bearer token is required", httpserver.RequestID(r.Context()))
			return
		}
		principal, err := verify(r.Context(), parts[1])
		if err != nil {
			api.WriteError(w, http.StatusUnauthorized, "AUTH_REQUIRED", "the bearer token is invalid", httpserver.RequestID(r.Context()))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalContextKey{}, principal)))
	})
}

func GinMiddleware(verify VerifyFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.Request.Header.Get("Authorization")
		parts := strings.Fields(header)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
			api.WriteError(c.Writer, http.StatusUnauthorized, "AUTH_REQUIRED", "a valid bearer token is required", httpserver.RequestID(c.Request.Context()))
			c.Abort()
			return
		}
		principal, err := verify(c.Request.Context(), parts[1])
		if err != nil {
			api.WriteError(c.Writer, http.StatusUnauthorized, "AUTH_REQUIRED", "the bearer token is invalid", httpserver.RequestID(c.Request.Context()))
			c.Abort()
			return
		}
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), principalContextKey{}, principal))
		c.Next()
	}
}
