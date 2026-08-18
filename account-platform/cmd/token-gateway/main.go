package main

import (
	"context"
	"encoding/base64"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

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
	secretProvider, err := token.NewSecretProvider(token.SecretProviderConfig{
		Environment: cfg.Environment, Mode: os.Getenv("TOKEN_SECRET_PROVIDER"), KeyID: os.Getenv("TOKEN_SECRET_KEY_ID"), LocalKey: key,
		RemoteURL: os.Getenv("TOKEN_SECRET_PROVIDER_URL"), RemoteToken: os.Getenv("TOKEN_SECRET_PROVIDER_TOKEN"),
	})
	if err != nil {
		log.Fatal(err)
	}
	repository, err := token.NewPostgresRepository(ctx, os.Getenv("DATABASE_URL"), secretProvider)
	if err != nil {
		log.Fatal(err)
	}
	defer repository.Close()
	verify, err := identity.NewOIDCVerifyFuncWithJWKS(ctx, os.Getenv("OIDC_ISSUER"), os.Getenv("OIDC_JWKS_URL"), os.Getenv("OIDC_AUDIENCE"), cfg.Environment)
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
	defaultPriority := 10
	if raw := strings.TrimSpace(os.Getenv("TOKEN_DEFAULT_ROUTE_PRIORITY")); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed < 0 || parsed > 10000 {
			log.Fatal("TOKEN_DEFAULT_ROUTE_PRIORITY must be between 0 and 10000")
		}
		defaultPriority = parsed
	}
	defaultRoutes, err := token.NewDefaultRouteProvisioner(repository, providerURLPolicy, token.DefaultRouteConfig{
		Provider: os.Getenv("TOKEN_DEFAULT_PROVIDER"),
		BaseURL:  os.Getenv("TOKEN_DEFAULT_PROVIDER_BASE_URL"),
		Secret:   []byte(os.Getenv("TOKEN_DEFAULT_PROVIDER_API_KEY")),
		Models:   strings.Split(os.Getenv("TOKEN_DEFAULT_PROVIDER_MODELS"), ","),
		Priority: defaultPriority,
	})
	if err != nil {
		log.Fatal("default Token Account provisioning is not configured: ", err)
	}
	reconciliationUserID, err := strconv.ParseInt(strings.TrimSpace(os.Getenv("TOKEN_RECONCILIATION_NEWAPI_ADMIN_USER_ID")), 10, 64)
	if err != nil || reconciliationUserID <= 0 {
		log.Fatal("TOKEN_RECONCILIATION_NEWAPI_ADMIN_USER_ID must be a positive integer")
	}
	reconciliationURL := strings.TrimSpace(os.Getenv("TOKEN_RECONCILIATION_NEWAPI_BASE_URL"))
	if reconciliationURL == "" {
		reconciliationURL = os.Getenv("TOKEN_DEFAULT_PROVIDER_BASE_URL")
	}
	evidenceClient, err := token.NewNewAPIUsageEvidenceClient(reconciliationURL, os.Getenv("TOKEN_RECONCILIATION_NEWAPI_ADMIN_TOKEN"), reconciliationUserID, providerURLPolicy)
	if err != nil {
		log.Fatal("New API usage reconciliation is not configured: ", err)
	}
	reconciliationInterval := 15 * time.Second
	if raw := strings.TrimSpace(os.Getenv("TOKEN_RECONCILIATION_INTERVAL_SECONDS")); raw != "" {
		seconds, parseErr := strconv.Atoi(raw)
		if parseErr != nil || seconds < 1 || seconds > 300 {
			log.Fatal("TOKEN_RECONCILIATION_INTERVAL_SECONDS must be between 1 and 300")
		}
		reconciliationInterval = time.Duration(seconds) * time.Second
	}
	workerContext, stopWorker := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopWorker()
	go token.NewUsageReconciler(repository, billingClient, evidenceClient, reconciliationInterval).Run(workerContext)
	service := token.NewService(repository, contexts, wallets, token.NewHTTPModelValidator(providerURLPolicy), providerURLPolicy, billingClient, token.NewHTTPUpstreamClient(), maximumInputTokens).WithDefaultRoutes(defaultRoutes)
	if err := httpserver.RunWithApplication("token-gateway", service.Handler(verify), repository.Ping); err != nil {
		log.Fatal(err)
	}
}
