package main

import (
	"context"
	"encoding/base64"
	"errors"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/requestdeadline"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/Pinvou/pinvou3/account-platform/internal/token"
)

func main() {
	if err := httpserver.ConfigureProcessLogging("token-gateway"); err != nil {
		log.Fatal("TOKEN_STARTUP_CONFIGURATION_FAILED")
	}
	ctx := context.Background()
	cfg, err := config.Load("token-gateway")
	if err != nil {
		log.Fatal("TOKEN_STARTUP_CONFIGURATION_FAILED")
	}
	key, err := base64.StdEncoding.DecodeString(os.Getenv("TOKEN_LOCAL_KEK_BASE64"))
	if err != nil {
		log.Fatal("TOKEN_LOCAL_KEK_BASE64 must be valid base64")
	}
	secretProvider, err := token.NewSecretProvider(token.SecretProviderConfig{
		Environment: cfg.Environment, Mode: os.Getenv("TOKEN_SECRET_PROVIDER"), KeyID: os.Getenv("TOKEN_SECRET_KEY_ID"), LocalKey: key,
		RemoteURL: os.Getenv("TOKEN_SECRET_PROVIDER_URL"), RemoteToken: os.Getenv("TOKEN_SECRET_PROVIDER_TOKEN"),
		RemoteTokenFile: os.Getenv("TOKEN_SECRET_PROVIDER_TOKEN_FILE"),
		TransitCAFile:   os.Getenv("TOKEN_SECRET_PROVIDER_CA_FILE"),
	})
	if err != nil {
		log.Fatal("TOKEN_STARTUP_CONFIGURATION_FAILED")
	}
	repository, err := token.NewPostgresRepository(ctx, os.Getenv("DATABASE_URL"), secretProvider)
	if err != nil {
		log.Fatal("TOKEN_STARTUP_CONFIGURATION_FAILED")
	}
	defer repository.Close()
	if err := repository.VerifyManagementMigrations(ctx, valueOrDefault("TOKEN_MIGRATIONS_DIR", "migrations/token")); err != nil {
		log.Fatal("TOKEN_MANAGEMENT_DATABASE_STAGE_INVALID")
	}
	paginationEnabled, err := strconv.ParseBool(valueOrDefault("TOKEN_ADMIN_PAGINATION_ENABLED", "false"))
	if err != nil {
		log.Fatal("TOKEN_ADMIN_PAGINATION_ENABLED must be true or false")
	}
	paginationKey := []byte(nil)
	if raw := strings.TrimSpace(os.Getenv("TOKEN_ADMIN_PAGINATION_CURSOR_HMAC_KEY_BASE64")); raw != "" {
		paginationKey, err = base64.StdEncoding.DecodeString(raw)
		if err != nil {
			log.Fatal("TOKEN_ADMIN_PAGINATION_CURSOR_HMAC_KEY_BASE64 must be valid base64")
		}
	}
	if err := repository.ConfigureAdminPagination(token.AdminPaginationConfig{Enabled: paginationEnabled, CursorHMACKey: paginationKey}); err != nil {
		log.Fatal("TOKEN_STARTUP_CONFIGURATION_FAILED")
	}
	if err := repository.SetAdminPaginationGate(ctx, paginationEnabled); err != nil {
		log.Fatal("TOKEN_STARTUP_CONFIGURATION_FAILED")
	}
	impactPrechecksEnabled, err := strconv.ParseBool(valueOrDefault("TOKEN_ADMIN_IMPACT_PRECHECKS_ENABLED", "false"))
	if err != nil {
		log.Fatal("TOKEN_ADMIN_IMPACT_PRECHECKS_ENABLED must be true or false")
	}
	operationsOverviewEnabled, err := strconv.ParseBool(valueOrDefault("TOKEN_ADMIN_OPERATIONS_OVERVIEW_ENABLED", "false"))
	if err != nil {
		log.Fatal("TOKEN_ADMIN_OPERATIONS_OVERVIEW_ENABLED must be true or false")
	}
	pageOverviewsEnabled, err := strconv.ParseBool(valueOrDefault("TOKEN_ADMIN_PAGE_OVERVIEWS_ENABLED", "false"))
	if err != nil {
		log.Fatal("TOKEN_ADMIN_PAGE_OVERVIEWS_ENABLED must be true or false")
	}
	if pageOverviewsEnabled && len(paginationKey) < 32 {
		log.Fatal("TOKEN_ADMIN_PAGE_OVERVIEWS_ENABLED requires TOKEN_ADMIN_PAGINATION_CURSOR_HMAC_KEY_BASE64 with at least 32 decoded bytes")
	}
	credentialIsolationEnabled, err := strconv.ParseBool(valueOrDefault("TOKEN_CREDENTIAL_ISOLATION_ENABLED", "false"))
	if err != nil {
		log.Fatal("TOKEN_CREDENTIAL_ISOLATION_ENABLED must be true or false")
	}
	verify, err := identity.NewOIDCVerifyFuncWithJWKS(ctx, os.Getenv("OIDC_ISSUER"), os.Getenv("OIDC_JWKS_URL"), os.Getenv("OIDC_AUDIENCE"), cfg.Environment)
	if err != nil {
		log.Fatal("TOKEN_STARTUP_CONFIGURATION_FAILED")
	}
	contexts, err := token.NewHTTPContextResolver(os.Getenv("ACCOUNT_HTTP_URL"))
	if err != nil {
		log.Fatal("TOKEN_STARTUP_CONFIGURATION_FAILED")
	}
	serviceCredential := serviceauth.CredentialFromEnvironment()
	authorizer, err := authorization.NewClient(os.Getenv("ACCOUNT_HTTP_URL"), serviceCredential)
	if err != nil {
		log.Fatal("TOKEN_STARTUP_CONFIGURATION_FAILED")
	}
	governance, err := repository.InstallProductionGovernance(authorizer)
	if err != nil {
		log.Fatal("TOKEN_STARTUP_CONFIGURATION_FAILED")
	}
	ownerAuthority, err := token.NewHTTPOwnerAuthorityClient(os.Getenv("ACCOUNT_HTTP_URL"), serviceCredential)
	if err != nil {
		log.Fatal("TOKEN_STARTUP_CONFIGURATION_FAILED")
	}
	ownerCandidates, err := token.NewHTTPOwnerCandidateDirectory(os.Getenv("ACCOUNT_HTTP_URL"), serviceCredential)
	if err != nil {
		log.Fatal("TOKEN_STARTUP_CONFIGURATION_FAILED")
	}
	wallets, err := token.NewHTTPWalletResolver(os.Getenv("BILLING_HTTP_URL"), serviceCredential)
	if err != nil {
		log.Fatal("TOKEN_STARTUP_CONFIGURATION_FAILED")
	}
	billingClient, err := token.NewHTTPBillingClient(os.Getenv("BILLING_HTTP_URL"), serviceCredential)
	if err != nil {
		log.Fatal("TOKEN_STARTUP_CONFIGURATION_FAILED")
	}
	providerURLPolicy, err := token.NewProviderURLPolicy(cfg.Environment, strings.Split(os.Getenv("TOKEN_PROVIDER_ALLOWED_HOSTS"), ","))
	if err != nil {
		log.Fatal("TOKEN_STARTUP_CONFIGURATION_FAILED")
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
	adapterName := strings.ToUpper(strings.TrimSpace(os.Getenv("TOKEN_GATEWAY_ADAPTER")))
	if adapterName == "" {
		adapterName = "NEW_API"
	}
	configuredAdapterVersion := map[string]string{"NEW_API": "new-api-v1", "OPENAI_COMPATIBLE": "openai-compatible-v1"}[adapterName]
	if configuredAdapterVersion == "" {
		log.Fatal("TOKEN_GATEWAY_ADAPTER must be NEW_API or OPENAI_COMPATIBLE")
	}
	defaultRoutes, err := token.NewDefaultRouteProvisioner(repository, providerURLPolicy, token.DefaultRouteConfig{
		Provider:       defaultProvider,
		AdapterVersion: configuredAdapterVersion,
		BaseURL:        os.Getenv("TOKEN_DEFAULT_PROVIDER_BASE_URL"),
		Secret:         []byte(os.Getenv("TOKEN_DEFAULT_PROVIDER_API_KEY")),
		Models:         strings.Split(os.Getenv("TOKEN_DEFAULT_PROVIDER_MODELS"), ","),
		Priority:       defaultPriority,
	})
	if err != nil {
		log.Fatal("TOKEN_STARTUP_CONFIGURATION_FAILED")
	}
	var gatewayAdapter token.ManagementCatalogAdapter
	switch adapterName {
	case "NEW_API":
		gatewayAdapter, err = token.NewNewAPIGatewayAdapterWithCA(os.Getenv("TOKEN_PROVIDER_CA_FILE"))
		if err != nil {
			log.Fatal("TOKEN_STARTUP_CONFIGURATION_FAILED")
		}
	case "OPENAI_COMPATIBLE":
		gatewayAdapter, err = token.NewOpenAICompatibleGatewayAdapterWithCA(os.Getenv("TOKEN_PROVIDER_CA_FILE"))
		if err != nil {
			log.Fatal("TOKEN_PROVIDER_TRUST_CONFIGURATION_FAILED")
		}
	default:
		log.Fatal("TOKEN_GATEWAY_ADAPTER must be NEW_API or OPENAI_COMPATIBLE")
	}
	catalogContextLength := int64(128_000)
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
		log.Fatal("TOKEN_STARTUP_CONFIGURATION_FAILED")
	}
	probeAdapter, ok := gatewayAdapter.(token.RouteHealthProbeAdapter)
	if !ok {
		log.Fatal("model gateway adapter does not support server-side route health probes")
	}
	routeHealth, err := token.NewRouteHealthWorker(repository, probeAdapter, providerURLPolicy, time.Second)
	if err != nil {
		log.Fatal("TOKEN_STARTUP_CONFIGURATION_FAILED")
	}
	workerContext, stopWorker := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopWorker()
	go observability.RunBackground(workerContext, "admin-pagination-cleanup", token.NewAdminPaginationCleanupWorker(repository, time.Minute).Run)
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
		log.Fatal("TOKEN_STARTUP_CONFIGURATION_FAILED")
	}
	go observability.RunBackground(workerContext, "secret-rotation", secretRotation.Run)
	initialSyncContext, cancelInitialSync := context.WithTimeout(workerContext, 30*time.Second)
	if _, _, syncErr := catalogSync.Sync(initialSyncContext); syncErr != nil {
		log.Print("TOKEN_CATALOG_INITIAL_SYNCHRONIZATION_DEFERRED")
	}
	cancelInitialSync()
	go observability.RunBackground(workerContext, "model-catalog-sync", catalogSync.Run)
	go observability.RunBackground(workerContext, "default-route-provisioning", defaultRoutes.Run)
	go observability.RunBackground(workerContext, "route-health-probe", routeHealth.Run)
	validator, err := token.NewHTTPModelValidatorWithCA(providerURLPolicy, os.Getenv("TOKEN_PROVIDER_CA_FILE"))
	if err != nil {
		log.Fatal("TOKEN_PROVIDER_VALIDATION_TRUST_CONFIGURATION_FAILED")
	}
	service := token.NewManagementService(repository, contexts, wallets, validator, providerURLPolicy, billingClient, gatewayAdapter).WithDefaultRoutes(defaultRoutes).WithCatalogSynchronizer(catalogSync).WithAuthorizer(authorizer).WithOwnerCandidateDirectory(ownerCandidates).WithAdminProxyHMACKey(paginationKey).WithImpactPrechecks(impactPrechecksEnabled, repository, billingClient).WithAdminOperationsOverview(operationsOverviewEnabled).WithAdminPageOverviews(pageOverviewsEnabled).WithCredentialIsolation(credentialIsolationEnabled).WithApprovedTokenCommandExecutor(token.NewPostgresApprovedCredentialExecutor(repository))
	service.WithTokenCommandGovernanceActivation(governance).WithTokenAccountProvisioning(wallets, ownerAuthority)
	if err := installPersonalExecution(workerContext, service, repository, billingClient, gatewayAdapter, defaultRoutes, providerURLPolicy, cfg.Environment, serviceCredential); err != nil {
		log.Fatal("TOKEN_PERSONAL_EXECUTION_CONFIGURATION_FAILED")
	}
	governedWorkers, err := token.NewTokenGovernanceSupervisor(service, repository, governance, wallets, ownerAuthority)
	if err != nil {
		log.Fatal("TOKEN_STARTUP_CONFIGURATION_FAILED")
	}
	var governanceStopped atomic.Bool
	go observability.RunBackground(workerContext, "token-governance-supervisor", func(ctx context.Context) {
		runRequiredGovernanceSupervisor(ctx, stopWorker, &governanceStopped, governedWorkers.Run)
	})
	go observability.RunBackground(workerContext, "model-emergency-cancellation", token.NewEmergencyModelCancellationWorker(repository, service, 5*time.Second).Run)
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
	handler := service.ManagementHandler(serviceCredential, verify)
	handler = requestdeadline.TokenAdminMiddleware(handler, time.Now)
	handler = token.TokenCredentialDeadlineMiddleware(handler)
	previousReady := ready
	ready = func(ctx context.Context) error {
		if governanceStopped.Load() {
			return errors.New("Token governance supervisor is not running")
		}
		if err := previousReady(ctx); err != nil {
			return err
		}
		_, err := governance.Snapshot(ctx)
		return err
	}
	collectors := append(repository.AdminPaginationCollectors(), service.AdminOperationsOverviewCollectors()...)
	if err := httpserver.RunWithApplicationContext(workerContext, "token-gateway", handler, ready, collectors...); err != nil {
		log.Fatal("TOKEN_STARTUP_CONFIGURATION_FAILED")
	}
	if governanceStopped.Load() {
		log.Fatal("TOKEN_GOVERNANCE_SUPERVISOR_STOPPED")
	}
}

func runRequiredGovernanceSupervisor(ctx context.Context, stop context.CancelFunc, stopped *atomic.Bool, run func(context.Context) error) {
	err := run(ctx)
	if ctx.Err() != nil && (err == nil || errors.Is(err, ctx.Err())) {
		return // Normal shutdown already owns HTTP and all background workers.
	}
	// Any unexpected return (including nil) loses the required worker owner.
	// Publish not-ready before cancelling HTTP; main then exits nonzero.
	stopped.Store(true)
	log.Print("TOKEN_GOVERNANCE_SUPERVISOR_STOPPED")
	stop()
}

func valueOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
