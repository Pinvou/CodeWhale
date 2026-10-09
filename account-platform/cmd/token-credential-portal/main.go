package main

import (
	"context"
	"encoding/base64"
	"errors"
	"log"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/Pinvou/pinvou3/account-platform/internal/token"
)

func main() {
	if err := httpserver.ConfigureProcessLogging("token-credential-portal"); err != nil {
		log.Fatal("CREDENTIAL_PORTAL_STARTUP_CONFIGURATION_FAILED")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cfg, err := config.Load("token-credential-portal")
	if err != nil {
		log.Fatal("CREDENTIAL_PORTAL_STARTUP_CONFIGURATION_FAILED")
	}
	key, err := base64.StdEncoding.DecodeString(os.Getenv("TOKEN_LOCAL_KEK_BASE64"))
	if err != nil {
		log.Fatal("TOKEN_LOCAL_KEK_BASE64 must be valid base64")
	}
	secrets, err := token.NewSecretProvider(token.SecretProviderConfig{
		Environment: cfg.Environment, Mode: os.Getenv("TOKEN_SECRET_PROVIDER"), KeyID: os.Getenv("TOKEN_SECRET_KEY_ID"), LocalKey: key,
		RemoteURL: os.Getenv("TOKEN_SECRET_PROVIDER_URL"), RemoteToken: os.Getenv("TOKEN_SECRET_PROVIDER_TOKEN"),
		RemoteTokenFile: os.Getenv("TOKEN_SECRET_PROVIDER_TOKEN_FILE"), TransitCAFile: os.Getenv("TOKEN_SECRET_PROVIDER_CA_FILE"),
	})
	if err != nil {
		log.Fatal("CREDENTIAL_PORTAL_STARTUP_CONFIGURATION_FAILED")
	}
	repository, err := token.NewPostgresRepository(ctx, os.Getenv("DATABASE_URL"), secrets)
	if err != nil {
		log.Fatal("CREDENTIAL_PORTAL_STARTUP_CONFIGURATION_FAILED")
	}
	defer repository.Close()
	if err := repository.VerifyManagementMigrations(ctx, credentialValueOrDefault("TOKEN_MIGRATIONS_DIR", "migrations/token")); err != nil {
		log.Fatal("CREDENTIAL_PORTAL_DATABASE_STAGE_INVALID")
	}
	authorizer, err := authorization.NewClient(os.Getenv("ACCOUNT_HTTP_URL"), serviceauth.CredentialFromEnvironment())
	if err != nil {
		log.Fatal("CREDENTIAL_PORTAL_STARTUP_CONFIGURATION_FAILED")
	}
	governance, err := repository.InstallProductionGovernance(authorizer)
	if err != nil {
		log.Fatal("CREDENTIAL_PORTAL_STARTUP_CONFIGURATION_FAILED")
	}
	providerURLs, err := token.NewProviderURLPolicy(cfg.Environment, strings.Split(os.Getenv("TOKEN_PROVIDER_ALLOWED_HOSTS"), ","))
	if err != nil {
		log.Fatal("CREDENTIAL_PORTAL_STARTUP_CONFIGURATION_FAILED")
	}
	issuerURL := mustURL("OIDC_ISSUER")
	jwksURL := mustURL("OIDC_JWKS_URL")
	verify, err := token.NewCredentialOIDCVerifyFunc(ctx, issuerURL.String(), jwksURL.String(), os.Getenv("CREDENTIAL_OIDC_AUDIENCE"), cfg.Environment)
	if err != nil {
		log.Fatal("CREDENTIAL_PORTAL_STARTUP_CONFIGURATION_FAILED")
	}
	publicURL := mustURL("CREDENTIAL_PORTAL_PUBLIC_URL")
	validator, err := token.NewHTTPModelValidatorWithCA(providerURLs, os.Getenv("TOKEN_PROVIDER_CA_FILE"))
	if err != nil {
		log.Fatal("CREDENTIAL_PORTAL_PROVIDER_TRUST_INVALID")
	}
	service, err := token.NewCredentialOriginService(token.CredentialOriginConfig{PublicURL: publicURL, PortalOrigin: mustURL("PORTAL_PUBLIC_URL"), IssuerURL: issuerURL, AuthorizationURL: mustURL("CREDENTIAL_OIDC_AUTHORIZATION_URL"), TokenURL: mustURL("CREDENTIAL_OIDC_TOKEN_URL"), JWKSURL: jwksURL, ClientID: os.Getenv("CREDENTIAL_OIDC_CLIENT_ID"), Verify: verify, SecureCookies: publicURL.Scheme == "https" || strings.HasPrefix(strings.ToLower(cfg.Environment), "production")}, repository, validator, providerURLs)
	if err != nil {
		log.Fatal("CREDENTIAL_PORTAL_STARTUP_CONFIGURATION_FAILED")
	}
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				janitorCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
				_, _ = repository.ExpireCredentialIsolation(janitorCtx, 100)
				cancel()
			}
		}
	}()
	ready := func(ctx context.Context) error {
		if err := repository.Ping(ctx); err != nil {
			return err
		}
		_, err := governance.Snapshot(ctx)
		return err
	}
	if err := httpserver.RunWithApplicationContext(ctx, "token-credential-portal", token.CredentialAuthorityDeadlineMiddleware(service.Handler()), ready); err != nil {
		log.Fatal("CREDENTIAL_PORTAL_STARTUP_CONFIGURATION_FAILED")
	}
}

func mustURL(name string) *url.URL {
	parsed, err := credentialURL(os.Getenv(name))
	if err != nil {
		log.Fatal("CREDENTIAL_PORTAL_ENDPOINT_CONFIGURATION_INVALID")
	}
	return parsed
}

func credentialURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, errors.New("invalid credential endpoint")
	}
	return parsed, nil
}

func credentialValueOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
