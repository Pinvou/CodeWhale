package main

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/clientcompat"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/manualreview"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/token"
	"os"
	"strconv"
	"strings"
	"time"
)

// Attach the invocation plane to the reviewed management host; it never replaces
// the management repository, safe discovery adapter or cancellation fund recovery.
func installPersonalExecution(ctx context.Context, service *token.Service, repository *token.PostgresRepository, billing *token.HTTPBillingClient, adapter token.ManagementCatalogAdapter, routes *token.PostgresDefaultRouteProvisioner, policy token.ProviderURLPolicy, environment, serviceCredential string) error {
	upstream, ok := adapter.(token.UpstreamClient)
	if !ok {
		return errors.New("personal execution adapter is incomplete")
	}
	maximumInput, err := executionPositiveInteger("TOKEN_GATEWAY_MAX_INPUT_TOKENS", 128000)
	if err != nil {
		return err
	}
	if _, err = service.WithPersonalModelExecution(repository, billing, upstream, maximumInput, 4096, 128000); err != nil {
		return err
	}
	compatibility, err := clientcompat.FromEnv()
	if err != nil {
		return errors.New("client compatibility configuration is invalid")
	}
	key, err := token.DecodeClientReadinessSigningKey(os.Getenv("CLIENT_READINESS_ATTESTATION_PRIVATE_KEY_BASE64"))
	if err != nil {
		return err
	}
	production := strings.EqualFold(environment, "production") || strings.EqualFold(environment, "production-new")
	if production && len(key) == 0 {
		return errors.New("client readiness signing key is required")
	}
	ttlSeconds, err := executionPositiveInteger("CLIENT_READINESS_ATTESTATION_TTL_SECONDS", 600)
	if err != nil || ttlSeconds < 60 || ttlSeconds > 900 {
		return errors.New("client readiness TTL must be between 60 and 900 seconds")
	}
	readiness, err := token.NewClientReadinessService(token.ClientReadinessConfig{Environment: environment, ServiceVersion: valueOrDefault("TOKEN_SERVICE_VERSION", "development"), ContractVersion: valueOrDefault("TOKEN_CLIENT_CONTRACT_VERSION", "client-readiness-v1"), MinimumClient: valueOrDefault("TOKEN_MINIMUM_CLIENT_VERSION", "0.8.0"), MaximumClient: valueOrDefault("TOKEN_MAXIMUM_CLIENT_VERSION", "0.8.0"), AccountURL: os.Getenv("ACCOUNT_HTTP_URL"), OIDCIssuer: os.Getenv("OIDC_ISSUER"), ServiceToken: serviceCredential, SigningKeyID: os.Getenv("CLIENT_READINESS_ATTESTATION_KEY_ID"), SigningKey: key, AttestationTTL: time.Duration(ttlSeconds) * time.Second, Compatibility: compatibility.Manifest()}, repository, routes, billing, repository)
	if err != nil {
		return err
	}
	service.WithClientReadiness(readiness)
	var evidence token.UsageEvidenceProvider
	if strings.EqualFold(os.Getenv("TOKEN_GATEWAY_ADAPTER"), "NEW_API") || strings.TrimSpace(os.Getenv("TOKEN_GATEWAY_ADAPTER")) == "" {
		user, err := executionPositiveInteger("TOKEN_RECONCILIATION_NEWAPI_ADMIN_USER_ID", 0)
		if err != nil {
			return err
		}
		url := valueOrDefault("TOKEN_RECONCILIATION_NEWAPI_BASE_URL", os.Getenv("TOKEN_DEFAULT_PROVIDER_BASE_URL"))
		evidence, err = token.NewNewAPIUsageEvidenceProviderWithCA(os.Getenv("TOKEN_DEFAULT_PROVIDER"), upstream.AdapterVersion(), url, os.Getenv("TOKEN_RECONCILIATION_NEWAPI_ADMIN_TOKEN"), user, os.Getenv("TOKEN_PROVIDER_CA_FILE"), policy)
		if err != nil {
			return err
		}
	}
	interval, err := executionPositiveInteger("TOKEN_RECONCILIATION_INTERVAL_SECONDS", 15)
	if err != nil || interval > 300 {
		return errors.New("usage reconciliation interval is invalid")
	}
	schedule, err := manualreview.ParseOnCallSchedule(os.Getenv("TOKEN_MANUAL_REVIEW_ONCALL_USER_IDS"), production)
	if err != nil {
		return err
	}
	go observability.RunBackground(ctx, "usage-reconciliation", token.NewUsageReconciler(repository, billing, evidence, time.Duration(interval)*time.Second).Run)
	go observability.RunBackground(ctx, "usage-replay-retention", token.NewReplayRetentionWorker(repository, 15*time.Second).Run)
	go observability.RunBackground(ctx, "usage-history-projection", token.NewUsageHistoryProjectionWorker(repository, 500).Run)
	go observability.RunBackground(ctx, "token-manual-review-sla", token.NewManualReviewSLAWorker(repository, schedule, time.Minute).Run)
	return nil
}
func executionPositiveInteger(name string, fallback int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		if fallback > 0 {
			return fallback, nil
		}
		return 0, errors.New("required execution configuration is missing")
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return 0, errors.New("execution configuration must be a positive integer")
	}
	return value, nil
}
