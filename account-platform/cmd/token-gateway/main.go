package main

import (
	"context"
	"log"
	"os"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/Pinvou/pinvou3/account-platform/internal/token"
)

func main() {
	if err := httpserver.ConfigureProcessLogging("token-gateway"); err != nil {
		log.Fatal("TOKEN_LOG_CONFIGURATION_FAILED")
	}
	cfg, err := config.Load("token-gateway")
	if err != nil {
		log.Fatal("TOKEN_RUNTIME_CONFIGURATION_FAILED")
	}
	credential := serviceauth.CredentialFromEnvironment()
	contexts, err := token.NewHTTPContextResolver(os.Getenv("ACCOUNT_HTTP_URL"))
	if err != nil {
		log.Fatal("TOKEN_ACCOUNT_CONTEXT_CONFIGURATION_FAILED")
	}
	wallets, err := token.NewHTTPWalletResolver(os.Getenv("BILLING_HTTP_URL"), credential)
	if err != nil {
		log.Fatal("TOKEN_WALLET_CONFIGURATION_FAILED")
	}
	balances, err := token.NewHTTPBillingClient(os.Getenv("BILLING_HTTP_URL"), credential)
	if err != nil {
		log.Fatal("TOKEN_BALANCE_CONFIGURATION_FAILED")
	}
	authorizer, err := authorization.NewClient(os.Getenv("ACCOUNT_HTTP_URL"), credential)
	if err != nil {
		log.Fatal("TOKEN_AUTHORIZATION_CONFIGURATION_FAILED")
	}
	verify, err := identity.NewOIDCVerifyFuncWithJWKS(context.Background(), os.Getenv("OIDC_ISSUER"), os.Getenv("OIDC_JWKS_URL"), os.Getenv("OIDC_AUDIENCE"), cfg.Environment)
	if err != nil {
		log.Fatal("TOKEN_USER_IDENTITY_CONFIGURATION_FAILED")
	}
	service := token.NewService(contexts, wallets, balances).WithAuthorizer(authorizer)
	if err := httpserver.RunWithApplication("token-gateway", service.Handler(verify), nil); err != nil {
		log.Fatal("TOKEN_SERVER_FAILED")
	}
}
