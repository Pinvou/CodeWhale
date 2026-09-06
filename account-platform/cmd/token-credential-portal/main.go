package main

import (
	"context"
	"encoding/base64"
	"log"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/clientcompat"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/Pinvou/pinvou3/account-platform/internal/token"
)

func main() {
	if err := httpserver.ConfigureProcessLogging("token-credential-portal"); err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cfg, err := config.Load("token-credential-portal")
	if err != nil {
		log.Fatal(err)
	}
	compatibility, err := clientcompat.FromEnv()
	if err != nil {
		log.Fatal(err)
	}
	key, err := base64.StdEncoding.DecodeString(os.Getenv("TOKEN_LOCAL_KEK_BASE64"))
	if err != nil {
		log.Fatal("TOKEN_LOCAL_KEK_BASE64 must be valid base64")
	}
	secrets, err := token.NewSecretProvider(token.SecretProviderConfig{Environment: cfg.Environment, Mode: os.Getenv("TOKEN_SECRET_PROVIDER"), KeyID: os.Getenv("TOKEN_SECRET_KEY_ID"), LocalKey: key, RemoteURL: os.Getenv("TOKEN_SECRET_PROVIDER_URL"), RemoteToken: os.Getenv("TOKEN_SECRET_PROVIDER_TOKEN")})
	if err != nil {
		log.Fatal(err)
	}
	repository, err := token.NewPostgresRepository(ctx, os.Getenv("DATABASE_URL"), secrets)
	if err != nil {
		log.Fatal(err)
	}
	defer repository.Close()
	authorizer, err := authorization.NewClient(os.Getenv("ACCOUNT_HTTP_URL"), serviceauth.CredentialFromEnvironment())
	if err != nil {
		log.Fatal("credential authority configuration is invalid: ", err)
	}
	governance, err := repository.InstallProductionGovernance(authorizer)
	if err != nil {
		log.Fatal("credential governance configuration is invalid: ", err)
	}
	providerURLs, err := token.NewProviderURLPolicy(cfg.Environment, strings.Split(os.Getenv("TOKEN_PROVIDER_ALLOWED_HOSTS"), ","))
	if err != nil {
		log.Fatal(err)
	}
	issuerURL := mustURL("OIDC_ISSUER")
	jwksURL := mustURL("OIDC_JWKS_URL")
	verify, err := token.NewCredentialOIDCVerifyFunc(ctx, issuerURL.String(), jwksURL.String(), os.Getenv("CREDENTIAL_OIDC_AUDIENCE"), cfg.Environment)
	if err != nil {
		log.Fatal(err)
	}
	publicURL := mustURL("CREDENTIAL_PORTAL_PUBLIC_URL")
	service, err := token.NewCredentialOriginService(token.CredentialOriginConfig{PublicURL: publicURL, PortalOrigin: mustURL("PORTAL_PUBLIC_URL"), IssuerURL: issuerURL, AuthorizationURL: mustURL("CREDENTIAL_OIDC_AUTHORIZATION_URL"), TokenURL: mustURL("CREDENTIAL_OIDC_TOKEN_URL"), JWKSURL: jwksURL, ClientID: os.Getenv("CREDENTIAL_OIDC_CLIENT_ID"), Verify: verify, SecureCookies: publicURL.Scheme == "https" || strings.HasPrefix(strings.ToLower(cfg.Environment), "production")}, repository, token.NewHTTPModelValidator(providerURLs), providerURLs)
	if err != nil {
		log.Fatal(err)
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
	if err := httpserver.RunWithApplication("token-credential-portal", token.CredentialAuthorityDeadlineMiddleware(compatibility.Wrap(service.Handler())), ready); err != nil {
		log.Fatal(err)
	}
}

func mustURL(name string) *url.URL {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(os.Getenv(name)), "/"))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		log.Fatalf("%s must be an absolute HTTP(S) URL", name)
	}
	return parsed
}
