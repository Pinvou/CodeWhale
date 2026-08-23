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

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/token"
)

func main() {
	if err := httpserver.ConfigureProcessLogging("token-gateway"); err != nil {
		log.Fatal(err)
	}
	if err := approvalpolicy.ValidateAll(map[string]approvalpolicy.Mode{"TOKEN_USAGE_RECONCILE": approvalpolicy.AuditOnly}); err != nil {
		log.Fatal(err)
	}
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
	authorizer, err := authorization.NewClient(os.Getenv("ACCOUNT_HTTP_URL"), os.Getenv("INTERNAL_SERVICE_TOKEN"))
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
	defaultProvider := strings.ToUpper(strings.TrimSpace(os.Getenv("TOKEN_DEFAULT_PROVIDER")))
	defaultRoutes, err := token.NewDefaultRouteProvisioner(repository, providerURLPolicy, token.DefaultRouteConfig{
		Provider: defaultProvider,
		BaseURL:  os.Getenv("TOKEN_DEFAULT_PROVIDER_BASE_URL"),
		Secret:   []byte(os.Getenv("TOKEN_DEFAULT_PROVIDER_API_KEY")),
		Models:   strings.Split(os.Getenv("TOKEN_DEFAULT_PROVIDER_MODELS"), ","),
		Priority: defaultPriority,
	})
	if err != nil {
		log.Fatal("default Token Account provisioning is not configured: ", err)
	}
	readinessSigningKey, err := token.DecodeClientReadinessSigningKey(os.Getenv("CLIENT_READINESS_ATTESTATION_PRIVATE_KEY_BASE64"))
	if err != nil {
		log.Fatal(err)
	}
	if (strings.EqualFold(cfg.Environment, "production") || strings.EqualFold(cfg.Environment, "production-new")) && len(readinessSigningKey) == 0 {
		log.Fatal("CLIENT_READINESS_ATTESTATION_PRIVATE_KEY_BASE64 is required in production")
	}
	readinessTTL := 10 * time.Minute
	if raw := strings.TrimSpace(os.Getenv("CLIENT_READINESS_ATTESTATION_TTL_SECONDS")); raw != "" {
		seconds, parseErr := strconv.Atoi(raw)
		if parseErr != nil || seconds < 60 || seconds > 900 {
			log.Fatal("CLIENT_READINESS_ATTESTATION_TTL_SECONDS must be between 60 and 900")
		}
		readinessTTL = time.Duration(seconds) * time.Second
	}
	readiness, err := token.NewClientReadinessService(token.ClientReadinessConfig{
		Environment: cfg.Environment, ServiceVersion: valueOrDefault("TOKEN_SERVICE_VERSION", "development"), ContractVersion: valueOrDefault("TOKEN_CLIENT_CONTRACT_VERSION", "client-readiness-v1"),
		MinimumClient: valueOrDefault("TOKEN_MINIMUM_CLIENT_VERSION", "0.8.0"), MaximumClient: valueOrDefault("TOKEN_MAXIMUM_CLIENT_VERSION", "0.8.0"),
		AccountURL: os.Getenv("ACCOUNT_HTTP_URL"), OIDCIssuer: os.Getenv("OIDC_ISSUER"), ServiceToken: os.Getenv("INTERNAL_SERVICE_TOKEN"),
		SigningKeyID: strings.TrimSpace(os.Getenv("CLIENT_READINESS_ATTESTATION_KEY_ID")), SigningKey: readinessSigningKey, AttestationTTL: readinessTTL,
	}, repository, defaultRoutes, billingClient, repository)
	if err != nil {
		log.Fatal("client readiness is not configured: ", err)
	}
	adapterName := strings.ToUpper(strings.TrimSpace(os.Getenv("TOKEN_GATEWAY_ADAPTER")))
	if adapterName == "" {
		adapterName = "NEW_API"
	}
	var gatewayAdapter token.UpstreamClient
	var evidenceClient token.UsageEvidenceProvider
	switch adapterName {
	case "NEW_API":
		gatewayAdapter = token.NewNewAPIGatewayAdapter()
		reconciliationUserID, parseErr := strconv.ParseInt(strings.TrimSpace(os.Getenv("TOKEN_RECONCILIATION_NEWAPI_ADMIN_USER_ID")), 10, 64)
		if parseErr != nil || reconciliationUserID <= 0 {
			log.Fatal("TOKEN_RECONCILIATION_NEWAPI_ADMIN_USER_ID must be a positive integer")
		}
		reconciliationURL := strings.TrimSpace(os.Getenv("TOKEN_RECONCILIATION_NEWAPI_BASE_URL"))
		if reconciliationURL == "" {
			reconciliationURL = os.Getenv("TOKEN_DEFAULT_PROVIDER_BASE_URL")
		}
		evidenceClient, err = token.NewNewAPIUsageEvidenceProvider(defaultProvider, gatewayAdapter.AdapterVersion(), reconciliationURL, os.Getenv("TOKEN_RECONCILIATION_NEWAPI_ADMIN_TOKEN"), reconciliationUserID, providerURLPolicy)
		if err != nil {
			log.Fatal("New API usage reconciliation is not configured: ", err)
		}
	case "OPENAI_COMPATIBLE":
		gatewayAdapter = token.NewHTTPUpstreamClient()
	default:
		log.Fatal("TOKEN_GATEWAY_ADAPTER must be NEW_API or OPENAI_COMPATIBLE")
	}
	reconciliationInterval := 15 * time.Second
	if raw := strings.TrimSpace(os.Getenv("TOKEN_RECONCILIATION_INTERVAL_SECONDS")); raw != "" {
		seconds, parseErr := strconv.Atoi(raw)
		if parseErr != nil || seconds < 1 || seconds > 300 {
			log.Fatal("TOKEN_RECONCILIATION_INTERVAL_SECONDS must be between 1 and 300")
		}
		reconciliationInterval = time.Duration(seconds) * time.Second
	}
	catalogContextLength := maximumInputTokens
	if raw := strings.TrimSpace(os.Getenv("TOKEN_MODEL_CATALOG_DEFAULT_CONTEXT_TOKENS")); raw != "" {
		parsed, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil || parsed <= 0 {
			log.Fatal("TOKEN_MODEL_CATALOG_DEFAULT_CONTEXT_TOKENS must be a positive integer")
		}
		catalogContextLength = parsed
	}
	catalogMaximumOutput := int64(4096)
	if raw := strings.TrimSpace(os.Getenv("TOKEN_MODEL_CATALOG_DEFAULT_MAX_OUTPUT_TOKENS")); raw != "" {
		parsed, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil || parsed < 0 || parsed > catalogContextLength {
			log.Fatal("TOKEN_MODEL_CATALOG_DEFAULT_MAX_OUTPUT_TOKENS must be within the catalog context limit")
		}
		catalogMaximumOutput = parsed
	}
	catalogInterval := 15 * time.Minute
	if raw := strings.TrimSpace(os.Getenv("TOKEN_MODEL_CATALOG_SYNC_MINUTES")); raw != "" {
		minutes, parseErr := strconv.Atoi(raw)
		if parseErr != nil || minutes < 1 || minutes > 1440 {
			log.Fatal("TOKEN_MODEL_CATALOG_SYNC_MINUTES must be between 1 and 1440")
		}
		catalogInterval = time.Duration(minutes) * time.Minute
	}
	catalogSync, err := token.NewCatalogSynchronizer(repository, gatewayAdapter, defaultProvider, os.Getenv("TOKEN_DEFAULT_PROVIDER_BASE_URL"), []byte(os.Getenv("TOKEN_DEFAULT_PROVIDER_API_KEY")), catalogContextLength, catalogMaximumOutput, catalogInterval)
	if err != nil {
		log.Fatal("model catalog synchronization is not configured: ", err)
	}
	workerContext, stopWorker := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopWorker()
	go observability.RunBackground(workerContext, "usage-reconciliation", token.NewUsageReconciler(repository, billingClient, evidenceClient, reconciliationInterval).Run)
	go observability.RunBackground(workerContext, "replay-retention", token.NewReplayRetentionWorker(repository, 15*time.Second).Run)
	secretRotationInterval := 5 * time.Minute
	if raw := strings.TrimSpace(os.Getenv("TOKEN_SECRET_ROTATION_INTERVAL_SECONDS")); raw != "" {
		seconds, parseErr := strconv.Atoi(raw)
		if parseErr != nil || seconds < 60 || seconds > 86400 {
			log.Fatal("TOKEN_SECRET_ROTATION_INTERVAL_SECONDS must be between 60 and 86400")
		}
		secretRotationInterval = time.Duration(seconds) * time.Second
	}
	secretRotationBatchSize := 100
	if raw := strings.TrimSpace(os.Getenv("TOKEN_SECRET_ROTATION_BATCH_SIZE")); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed < 1 || parsed > 1000 {
			log.Fatal("TOKEN_SECRET_ROTATION_BATCH_SIZE must be between 1 and 1000")
		}
		secretRotationBatchSize = parsed
	}
	secretRotation, err := token.NewSecretRotationWorker(repository, secretRotationInterval, secretRotationBatchSize)
	if err != nil {
		log.Fatal("secret rotation is not configured: ", err)
	}
	go observability.RunBackground(workerContext, "secret-rotation", secretRotation.Run)
	initialSyncContext, cancelInitialSync := context.WithTimeout(workerContext, 30*time.Second)
	if _, _, syncErr := catalogSync.Sync(initialSyncContext); syncErr != nil {
		log.Printf("model catalog initial synchronization deferred: %v", syncErr)
	}
	cancelInitialSync()
	go observability.RunBackground(workerContext, "model-catalog-sync", catalogSync.Run)
	go observability.RunBackground(workerContext, "default-route-provisioning", defaultRoutes.Run)
	service := token.NewService(repository, contexts, wallets, token.NewHTTPModelValidator(providerURLPolicy), providerURLPolicy, billingClient, gatewayAdapter, maximumInputTokens).WithDefaultRoutes(defaultRoutes).WithCatalogSynchronizer(catalogSync).WithAuthorizer(authorizer).WithClientReadiness(readiness)
	ready := repository.Ping
	if strings.EqualFold(cfg.Environment, "production") || strings.EqualFold(cfg.Environment, "production-new") {
		ready = func(ctx context.Context) error {
			if err := repository.Ping(ctx); err != nil {
				return err
			}
			remaining, err := repository.SecretRotationRemaining(ctx)
			if err != nil {
				return err
			}
			if remaining != 0 {
				return token.ErrSecretRotationPending
			}
			return nil
		}
	}
	if err := httpserver.RunWithApplication("token-gateway", service.Handler(os.Getenv("INTERNAL_SERVICE_TOKEN"), verify), ready); err != nil {
		log.Fatal(err)
	}
}

func valueOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
