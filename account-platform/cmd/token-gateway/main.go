package main

import (
	"context"
	"encoding/base64"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/token"
)

func main() {
	ctx := context.Background()
	cfg, err := config.Load("token-gateway")
	if err != nil {
		log.Fatal(err)
	}
	key, err := base64.StdEncoding.DecodeString(os.Getenv("TOKEN_LOCAL_KEK_BASE64"))
	if err != nil {
		log.Fatal("TOKEN_LOCAL_KEK_BASE64 must be valid base64")
	}
	secretProvider, err := token.NewSecretProvider(cfg.Environment, os.Getenv("TOKEN_SECRET_PROVIDER"), os.Getenv("TOKEN_SECRET_KEY_ID"), key)
	if err != nil {
		log.Fatal(err)
	}
	repository, err := token.NewPostgresRepository(ctx, os.Getenv("DATABASE_URL"), secretProvider)
	if err != nil {
		log.Fatal(err)
	}
	defer repository.Close()
	verify, err := identity.NewOIDCVerifyFunc(ctx, os.Getenv("OIDC_ISSUER"), os.Getenv("OIDC_AUDIENCE"), cfg.Environment, os.Getenv("OIDC_JWKS_URL"))
	if err != nil {
		log.Fatal(err)
	}
	contexts, err := token.NewHTTPContextResolver(os.Getenv("ACCOUNT_HTTP_URL"))
	if err != nil {
		log.Fatal(err)
	}
	wallets, err := token.NewHTTPWalletResolver(os.Getenv("BILLING_HTTP_URL"), os.Getenv("INTERNAL_SERVICE_TOKEN"))
	if err != nil {
		log.Fatal(err)
	}
	billingClient, err := token.NewHTTPBillingClient(os.Getenv("BILLING_HTTP_URL"), os.Getenv("INTERNAL_SERVICE_TOKEN"))
	if err != nil {
		log.Fatal(err)
	}
	maximumInputTokens := int64(128_000)
	if raw := os.Getenv("TOKEN_GATEWAY_MAX_INPUT_TOKENS"); raw != "" {
		parsed, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil || parsed <= 0 {
			log.Fatal("TOKEN_GATEWAY_MAX_INPUT_TOKENS must be a positive integer")
		}
		maximumInputTokens = parsed
	}
	providerURLPolicy, err := token.NewProviderURLPolicy(cfg.Environment, strings.Split(os.Getenv("TOKEN_PROVIDER_ALLOWED_HOSTS"), ","))
	if err != nil {
		log.Fatal(err)
	}
	service := token.NewService(repository, contexts, wallets, token.NewHTTPModelValidator(providerURLPolicy), providerURLPolicy, billingClient, token.NewHTTPUpstreamClient(), maximumInputTokens)
	if err := httpserver.RunWithApplication("token-gateway", service.Handler(verify), repository.Ping); err != nil {
		log.Fatal(err)
	}
}
