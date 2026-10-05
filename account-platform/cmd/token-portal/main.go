package main

import (
	"log"
	"net/url"
	"os"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/portal"
)

func main() {
	cfg, err := config.Load("token-portal")
	if err != nil {
		log.Fatal(err)
	}
	store, err := portal.NewRedisStore(os.Getenv("REDIS_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	publicURL := mustURL("PORTAL_PUBLIC_URL")
	service, err := portal.NewService(portal.Config{
		Environment:      cfg.Environment,
		IssuerURL:        mustURL("PORTAL_OIDC_ISSUER"),
		JWKSURL:          mustURL("PORTAL_OIDC_JWKS_URL"),
		PublicURL:        publicURL,
		AuthorizationURL: mustURL("PORTAL_OIDC_AUTHORIZATION_URL"),
		TokenURL:         mustURL("PORTAL_OIDC_TOKEN_URL"),
		ClientID:         os.Getenv("PORTAL_OIDC_CLIENT_ID"),
		AccountURL:       mustURL("ACCOUNT_HTTP_URL"),
		BillingURL:       mustURL("BILLING_HTTP_URL"),
		TokenGatewayURL:  mustURL("TOKEN_GATEWAY_HTTP_URL"),
		SecureCookies:    publicURL.Scheme == "https" || cfg.Environment != "development" && cfg.Environment != "test",
	}, store)
	if err != nil {
		log.Fatal(err)
	}
	if err := httpserver.RunWithApplication("token-portal", service.Handler(portal.StaticHandler()), store.Ping); err != nil {
		log.Fatal(err)
	}
}

func mustURL(name string) *url.URL {
	value := strings.TrimRight(strings.TrimSpace(os.Getenv(name)), "/")
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		log.Fatalf("%s must be an absolute URL", name)
	}
	return parsed
}
