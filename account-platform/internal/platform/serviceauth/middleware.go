package serviceauth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gin-gonic/gin"
)

const defaultMaximumLifetime = 15 * time.Minute

type Identity struct {
	ActorType   string
	Service     string
	Subject     string
	JobType     string
	JobRunID    string
	Permission  string
	permissions []string
}

type identityContextKey struct{}

type rule struct {
	Subject     string   `json:"subject"`
	Service     string   `json:"service"`
	Permissions []string `json:"permissions"`
}

type authenticator struct {
	legacyToken string
	verify      func(context.Context, string) (string, error)
	rules       []rule
	invalid     error
}

func FromContext(ctx context.Context) (Identity, bool) {
	identity, ok := ctx.Value(identityContextKey{}).(Identity)
	return identity, ok
}

func ContextForJob(ctx context.Context, service, jobType, jobRunID string) (context.Context, error) {
	service, jobType, jobRunID = strings.TrimSpace(service), strings.TrimSpace(jobType), strings.TrimSpace(jobRunID)
	if service == "" || jobType == "" || jobRunID == "" {
		return ctx, errors.New("service, job type, and job run ID are required")
	}
	permission := "internal.job." + strings.ToLower(strings.ReplaceAll(jobType, "_", "-"))
	if !validPermission(permission) {
		return ctx, errors.New("job type cannot be mapped to an exact internal permission")
	}
	identity := Identity{ActorType: "SERVICE", Service: service, Subject: service, JobType: jobType, JobRunID: jobRunID, Permission: permission, permissions: []string{permission}}
	return context.WithValue(ctx, identityContextKey{}, identity), nil
}

func CredentialFromEnvironment() string {
	if path := strings.TrimSpace(os.Getenv("WORKLOAD_IDENTITY_TOKEN_FILE")); path != "" {
		return "file:" + path
	}
	return strings.TrimSpace(os.Getenv("INTERNAL_SERVICE_TOKEN"))
}

func SetAuthorization(request *http.Request, credential string) error {
	credential = strings.TrimSpace(credential)
	if strings.HasPrefix(credential, "file:") {
		path := strings.TrimSpace(strings.TrimPrefix(credential, "file:"))
		if path == "" || !filepath.IsAbs(path) {
			return errors.New("workload identity token path must be absolute")
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read workload identity token: %w", err)
		}
		credential = strings.TrimSpace(string(body))
	}
	if credential == "" || len(credential) > 32*1024 || strings.ContainsAny(credential, "\r\n") {
		return errors.New("workload identity token is invalid")
	}
	request.Header.Set("Authorization", "Bearer "+credential)
	return nil
}

func Middleware(expectedToken string, next http.Handler) http.Handler {
	auth := newAuthenticator(expectedToken)
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		identity, status := auth.authenticate(request)
		if status != http.StatusOK {
			writeAuthenticationError(w, request, status)
			return
		}
		next.ServeHTTP(w, request.WithContext(context.WithValue(request.Context(), identityContextKey{}, identity)))
	})
}

func GinMiddleware(expectedToken string) gin.HandlerFunc {
	auth := newAuthenticator(expectedToken)
	return func(c *gin.Context) {
		identity, status := auth.authenticate(c.Request)
		if status != http.StatusOK {
			writeAuthenticationError(c.Writer, c.Request, status)
			c.Abort()
			return
		}
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), identityContextKey{}, identity))
		c.Next()
	}
}

func newAuthenticator(expectedToken string) authenticator {
	issuer := strings.TrimRight(strings.TrimSpace(os.Getenv("WORKLOAD_IDENTITY_ISSUER")), "/")
	caFile := strings.TrimSpace(os.Getenv("WORKLOAD_IDENTITY_CA_FILE"))
	if issuer == "" {
		if caFile != "" {
			return authenticator{invalid: errWorkloadTrust}
		}
		environment := strings.ToLower(strings.TrimSpace(os.Getenv("APP_ENV")))
		if environment != "" && environment != "development" && environment != "test" {
			return authenticator{invalid: errors.New("OIDC workload identity is required outside development and test")}
		}
		return authenticator{legacyToken: strings.TrimSpace(expectedToken)}
	}
	jwksURL := strings.TrimSpace(os.Getenv("WORKLOAD_IDENTITY_JWKS_URL"))
	audience := strings.TrimSpace(os.Getenv("WORKLOAD_IDENTITY_AUDIENCE"))
	rawRules := strings.TrimSpace(os.Getenv("WORKLOAD_IDENTITY_RULES_JSON"))
	if validateHTTPSURL(issuer) != nil || validateHTTPSURL(jwksURL) != nil || audience == "" || rawRules == "" {
		return authenticator{invalid: errors.New("workload identity issuer, JWKS URL, audience, and rules are required")}
	}
	var rules []rule
	if err := json.Unmarshal([]byte(rawRules), &rules); err != nil || len(rules) == 0 {
		return authenticator{invalid: errors.New("workload identity rules must be a non-empty JSON array")}
	}
	for _, item := range rules {
		if strings.TrimSpace(item.Subject) == "" || strings.TrimSpace(item.Service) == "" || len(item.Permissions) == 0 {
			return authenticator{invalid: errors.New("each workload identity rule requires subject, service, and exact permissions")}
		}
		for _, permission := range item.Permissions {
			if !validPermission(permission) {
				return authenticator{invalid: errors.New("workload identity permission must be an exact internal permission")}
			}
		}
	}
	if caFile != "" && (!canonicalWorkloadHTTPS(issuer) || !canonicalWorkloadHTTPS(jwksURL)) {
		return authenticator{invalid: errWorkloadTrust}
	}
	client, err := workloadHTTPClient(caFile)
	if err != nil {
		return authenticator{invalid: err}
	}
	keySet := oidc.NewRemoteKeySet(oidc.ClientContext(context.Background(), client), jwksURL)
	verifier := oidc.NewVerifier(issuer, keySet, &oidc.Config{ClientID: audience})
	return authenticator{rules: rules, verify: func(ctx context.Context, raw string) (string, error) {
		token, err := verifier.Verify(ctx, raw)
		if err != nil {
			return "", err
		}
		var claims struct {
			NotBefore int64 `json:"nbf"`
		}
		if token.Subject == "" || token.Claims(&claims) != nil || !validTokenTimes(token.IssuedAt, token.Expiry, claims.NotBefore, time.Now()) {
			return "", errors.New("workload token subject or validity interval is invalid")
		}
		return token.Subject, nil
	}}
}

func validTokenTimes(issued, expiry time.Time, notBefore int64, now time.Time) bool {
	lifetime := expiry.Sub(issued)
	return !issued.IsZero() && !issued.After(now) && expiry.After(now) && lifetime > 0 && lifetime <= defaultMaximumLifetime && (notBefore == 0 || !time.Unix(notBefore, 0).After(now))
}

func (auth authenticator) authenticate(request *http.Request) (Identity, int) {
	parts := strings.Fields(request.Header.Get("Authorization"))
	if auth.invalid != nil || len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
		return Identity{}, http.StatusUnauthorized
	}
	if auth.verify == nil {
		valid := auth.legacyToken != "" && len(parts[1]) == len(auth.legacyToken)
		if valid {
			valid = subtle.ConstantTimeCompare([]byte(parts[1]), []byte(auth.legacyToken)) == 1
		}
		if !valid {
			return Identity{}, http.StatusUnauthorized
		}
		return Identity{ActorType: "SERVICE", Service: "development-static-token", Subject: "development-static-token", permissions: []string{"internal.*"}}, http.StatusOK
	}
	subject, err := auth.verify(request.Context(), parts[1])
	if err != nil {
		return Identity{}, http.StatusUnauthorized
	}
	for _, item := range auth.rules {
		if item.Subject == subject {
			return Identity{ActorType: "SERVICE", Service: item.Service, Subject: subject, permissions: append([]string(nil), item.Permissions...)}, http.StatusOK
		}
	}
	return Identity{}, http.StatusForbidden
}

func Require(permission string) gin.HandlerFunc {
	permission = strings.TrimSpace(permission)
	return func(c *gin.Context) {
		workload, ok := FromContext(c.Request.Context())
		if !ok || !validPermission(permission) || (!slices.Contains(workload.permissions, permission) && !slices.Contains(workload.permissions, "internal.*")) {
			writeAuthenticationError(c.Writer, c.Request, http.StatusForbidden)
			c.Abort()
			return
		}
		workload.Permission = permission
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), identityContextKey{}, workload))
		c.Next()
	}
}

func validPermission(permission string) bool {
	permission = strings.TrimSpace(permission)
	return strings.HasPrefix(permission, "internal.") && permission != "internal.*" && !strings.ContainsAny(permission, "* \\/")
}

func validateHTTPSURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return errors.New("an absolute HTTPS URL is required")
	}
	return nil
}

func writeAuthenticationError(w http.ResponseWriter, request *http.Request, status int) {
	if status == http.StatusForbidden {
		api.WriteError(w, status, "SERVICE_AUTH_FORBIDDEN", "workload identity is not authorized for this operation", httpserver.RequestID(request.Context()))
		return
	}
	api.WriteError(w, http.StatusUnauthorized, "SERVICE_AUTH_REQUIRED", "valid service authentication is required", httpserver.RequestID(request.Context()))
}
