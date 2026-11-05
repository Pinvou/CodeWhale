package main

import (
	"context"
	"errors"
	"os"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/Pinvou/pinvou3/account-platform/internal/token"
)

// The existing personal facade has no management repository or model workers.
// Keep its original nil readiness contract: requests validate Account authority
// and Billing facts rather than introducing model startup/readiness dependencies.
func runPersonalGateway(ctx context.Context, environment string) error {
	verify, err := identity.NewOIDCVerifyFuncWithJWKS(ctx, os.Getenv("OIDC_ISSUER"), os.Getenv("OIDC_JWKS_URL"), os.Getenv("OIDC_AUDIENCE"), environment)
	if err != nil {
		return errors.New("TOKEN_PERSONAL_IDENTITY_CONFIGURATION_INVALID")
	}
	service, err := newPersonalGatewayService(os.Getenv("ACCOUNT_HTTP_URL"), os.Getenv("BILLING_HTTP_URL"), serviceauth.CredentialFromEnvironment())
	if err != nil {
		return err
	}
	return httpserver.RunWithApplication("token-gateway", service.Handler(verify), nil)
}

func newPersonalGatewayService(accountURL, billingURL, credential string) (*token.Service, error) {
	contexts, err := token.NewHTTPContextResolver(accountURL)
	if err != nil {
		return nil, errors.New("TOKEN_PERSONAL_CONTEXT_CONFIGURATION_INVALID")
	}
	authorizer, err := authorization.NewClient(accountURL, credential)
	if err != nil {
		return nil, errors.New("TOKEN_PERSONAL_AUTHORIZATION_CONFIGURATION_INVALID")
	}
	wallets, err := token.NewHTTPWalletResolver(billingURL, credential)
	if err != nil {
		return nil, errors.New("TOKEN_PERSONAL_WALLET_CONFIGURATION_INVALID")
	}
	billingClient, err := token.NewHTTPBillingClient(billingURL, credential)
	if err != nil {
		return nil, errors.New("TOKEN_PERSONAL_BILLING_CONFIGURATION_INVALID")
	}
	return token.NewService(contexts, wallets, billingClient).WithAuthorizer(authorizer), nil
}
