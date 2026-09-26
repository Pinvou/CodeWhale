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

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/clientcompat"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/manualreview"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/requestdeadline"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
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
	onCallSchedule, err := manualreview.ParseOnCallSchedule(os.Getenv("MANUAL_REVIEW_ON_CALL_USER_IDS"), strings.HasPrefix(strings.ToLower(cfg.Environment), "production"))
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
	secretProvider, err := token.NewSecretProvider(token.SecretProviderConfig{
		Environment: cfg.Environment, Mode: os.Getenv("TOKEN_SECRET_PROVIDER"), KeyID: os.Getenv("TOKEN_SECRET_KEY_ID"), LocalKey: key,
		RemoteURL: os.Getenv("TOKEN_SECRET_PROVIDER_URL"), RemoteToken: os.Getenv("TOKEN_SECRET_PROVIDER_TOKEN"),
		RemoteTokenFile: os.Getenv("TOKEN_SECRET_PROVIDER_TOKEN_FILE"),
		TransitCAFile:   os.Getenv("TOKEN_SECRET_PROVIDER_CA_FILE"),
	})
	if err != nil {
		log.Fatal(err)
	}
	repository, err := token.NewPostgresRepository(ctx, os.Getenv("DATABASE_URL"), secretProvider)
	if err != nil {
		log.Fatal(err)
	}
	defer repository.Close()
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
		log.Fatal("Token admin pagination is not configured: ", err)
	}
	if err := repository.SetAdminPaginationGate(ctx, paginationEnabled); err != nil {
		log.Fatal("Token admin pagination rollout gate could not be persisted: ", err)
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
		log.Fatal(err)
	}
	contexts, err := token.NewHTTPContextResolver(os.Getenv("ACCOUNT_HTTP_URL"))
	if err != nil {
		log.Fatal(err)
	}
	serviceCredential := serviceauth.CredentialFromEnvironment()
	authorizer, err := authorization.NewClient(os.Getenv("ACCOUNT_HTTP_URL"), serviceCredential)
	if err != nil {
		log.Fatal(err)
	}
	governance, err := repository.InstallProductionGovernance(authorizer)
	if err != nil {
		log.Fatal("Token governance configuration is invalid: ", err)
	}
	ownerAuthority, err := token.NewHTTPOwnerAuthorityClient(os.Getenv("ACCOUNT_HTTP_URL"), serviceCredential)
	if err != nil {
		log.Fatal("Token account authority configuration is invalid: ", err)
	}
	ownerCandidates, err := token.NewHTTPOwnerCandidateDirectory(os.Getenv("ACCOUNT_HTTP_URL"), serviceCredential)
	if err != nil {
		log.Fatal(err)
	}
	wallets, err := token.NewHTTPWalletResolver(os.Getenv("BILLING_HTTP_URL"), serviceCredential)
	if err != nil {
		log.Fatal(err)
	}
	billingClient, err := token.NewHTTPBillingClient(os.Getenv("BILLING_HTTP_URL"), serviceCredential)
	if err != nil {
		log.Fatal(err)
	}
	billingClient.WithHumanDelegations(authorizer)
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
		AccountURL: os.Getenv("ACCOUNT_HTTP_URL"), OIDCIssuer: os.Getenv("OIDC_ISSUER"), ServiceToken: serviceCredential,
		SigningKeyID: strings.TrimSpace(os.Getenv("CLIENT_READINESS_ATTESTATION_KEY_ID")), SigningKey: readinessSigningKey, AttestationTTL: readinessTTL,
		Compatibility: compatibility.Manifest(),
	}, repository, defaultRoutes, billingClient, repository)
	if err != nil {
		log.Fatal("client readiness is not configured: ", err)
	}
	var gatewayAdapter token.UpstreamClient
	var evidenceClient token.UsageEvidenceProvider
	switch adapterName {
	case "NEW_API":
		gatewayAdapter, err = token.NewNewAPIGatewayAdapterWithCA(os.Getenv("TOKEN_PROVIDER_CA_FILE"))
		if err != nil {
			log.Fatal("New API provider trust is not configured: ", err)
		}
		reconciliationUserID, parseErr := strconv.ParseInt(strings.TrimSpace(os.Getenv("TOKEN_RECONCILIATION_NEWAPI_ADMIN_USER_ID")), 10, 64)
		if parseErr != nil || reconciliationUserID <= 0 {
			log.Fatal("TOKEN_RECONCILIATION_NEWAPI_ADMIN_USER_ID must be a positive integer")
		}
		reconciliationURL := strings.TrimSpace(os.Getenv("TOKEN_RECONCILIATION_NEWAPI_BASE_URL"))
		if reconciliationURL == "" {
			reconciliationURL = os.Getenv("TOKEN_DEFAULT_PROVIDER_BASE_URL")
		}
		evidenceClient, err = token.NewNewAPIUsageEvidenceProviderWithCA(defaultProvider, gatewayAdapter.AdapterVersion(), reconciliationURL, os.Getenv("TOKEN_RECONCILIATION_NEWAPI_ADMIN_TOKEN"), reconciliationUserID, os.Getenv("TOKEN_PROVIDER_CA_FILE"), providerURLPolicy)
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
	probeAdapter, ok := gatewayAdapter.(token.RouteHealthProbeAdapter)
	if !ok {
		log.Fatal("model gateway adapter does not support server-side route health probes")
	}
	routeHealth, err := token.NewRouteHealthWorker(repository, probeAdapter, providerURLPolicy, time.Second)
	if err != nil {
		log.Fatal("route health worker is not configured: ", err)
	}
	workerContext, stopWorker := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopWorker()
	go observability.RunBackground(workerContext, "usage-reconciliation", token.NewUsageReconciler(repository, billingClient, evidenceClient, reconciliationInterval).Run)
	go observability.RunBackground(workerContext, "manual-review-sla", token.NewManualReviewSLAWorker(repository, onCallSchedule, time.Minute).Run)
	go observability.RunBackground(workerContext, "replay-retention", token.NewReplayRetentionWorker(repository, 15*time.Second).Run)
	go observability.RunBackground(workerContext, "admin-pagination-cleanup", token.NewAdminPaginationCleanupWorker(repository, time.Minute).Run)
	go observability.RunBackground(workerContext, "usage-history-projection", token.NewUsageHistoryProjectionWorker(repository, 500).Run)
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
	go observability.RunBackground(workerContext, "route-health-probe", routeHealth.Run)
	service := token.NewService(repository, contexts, wallets, token.NewHTTPModelValidator(providerURLPolicy), providerURLPolicy, billingClient, gatewayAdapter, maximumInputTokens).WithDefaultRoutes(defaultRoutes).WithCatalogSynchronizer(catalogSync).WithAuthorizer(authorizer).WithClientReadiness(readiness).WithOwnerCandidateDirectory(ownerCandidates).WithAdminProxyHMACKey(paginationKey).WithImpactPrechecks(impactPrechecksEnabled, repository, billingClient).WithAdminOperationsOverview(operationsOverviewEnabled).WithAdminPageOverviews(pageOverviewsEnabled).WithCredentialIsolation(credentialIsolationEnabled).WithApprovedTokenCommandExecutor(token.NewPostgresApprovedCredentialExecutor(repository))
	service.WithTokenCommandGovernanceActivation(governance).WithTokenAccountProvisioning(wallets, ownerAuthority)
	governedWorkers, err := token.NewTokenGovernanceSupervisor(service, repository, governance, wallets, ownerAuthority)
	if err != nil {
		log.Fatal("Token governed workers are not configured: ", err)
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
	handler := compatibility.Wrap(service.Handler(os.Getenv("INTERNAL_SERVICE_TOKEN"), verify))
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
		log.Fatal(err)
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
