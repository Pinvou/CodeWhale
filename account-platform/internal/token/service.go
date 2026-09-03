package token

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/resilience"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

type ContextResolver interface {
	Resolve(context.Context, string) (AuthorizationContext, error)
}

type CommerceCustomerResolver interface {
	ResolveCommerceCustomer(context.Context, string) (string, error)
}

type WalletResolver interface {
	Ensure(context.Context, string, uuid.UUID) (uuid.UUID, error)
}

type CredentialValidator interface {
	Validate(context.Context, string, []byte, []string) error
}

type Service struct {
	repository                  Repository
	contexts                    ContextResolver
	commerceCustomers           CommerceCustomerResolver
	wallets                     WalletResolver
	validator                   CredentialValidator
	providerURLs                ProviderURLPolicy
	billing                     BillingClient
	upstream                    UpstreamClient
	maximumInputTokens          int64
	defaultMaxOutputTokens      int64
	maximumOutputTokens         int64
	defaultRoutes               DefaultRouteProvisioner
	catalogSync                 *CatalogSynchronizer
	authorizer                  *authorization.Client
	clientReadiness             *ClientReadinessService
	ownerCandidates             OwnerCandidateDirectory
	commandGovernanceActivation TokenCommandGovernanceActivationReader
	impactPrechecksEnabled      bool
	impactPrechecks             ImpactPrecheckRepository
	impactPricing               ImpactPriceCoverageClient
	approvedCommandExecutor     ApprovedTokenCommandExecutor
	operationsOverviewEnabled   bool
	operationsOverviewMetrics   *tokenOperationsOverviewMetrics
	adminProxyHMACKey           []byte
	breaker                     *resilience.Breaker
	activeUpstreamCancels       sync.Map
}

func (s *Service) registerActiveUpstream(usageID uuid.UUID, cancel context.CancelFunc) func() {
	s.activeUpstreamCancels.Store(usageID, cancel)
	return func() { s.activeUpstreamCancels.Delete(usageID) }
}

func (s *Service) abortActiveUpstream(usageID uuid.UUID) bool {
	value, ok := s.activeUpstreamCancels.Load(usageID)
	if !ok {
		return false
	}
	cancel, ok := value.(context.CancelFunc)
	if ok {
		cancel()
	}
	return ok
}

func (s *Service) WithDefaultRoutes(provisioner DefaultRouteProvisioner) *Service {
	s.defaultRoutes = provisioner
	return s
}

func (s *Service) WithAuthorizer(authorizer *authorization.Client) *Service {
	s.authorizer = authorizer
	return s
}

func (s *Service) WithCatalogSynchronizer(synchronizer *CatalogSynchronizer) *Service {
	s.catalogSync = synchronizer
	return s
}

func (s *Service) WithClientReadiness(readiness *ClientReadinessService) *Service {
	s.clientReadiness = readiness
	return s
}

func (s *Service) WithOwnerCandidateDirectory(directory OwnerCandidateDirectory) *Service {
	s.ownerCandidates = directory
	return s
}

func (s *Service) WithAdminProxyHMACKey(key []byte) *Service {
	s.adminProxyHMACKey = append([]byte(nil), key...)
	return s
}

func (s *Service) WithApprovedTokenCommandExecutor(executor ApprovedTokenCommandExecutor) *Service {
	s.approvedCommandExecutor = executor
	return s
}

func (s *Service) WithAdminOperationsOverview(enabled bool) *Service {
	s.operationsOverviewEnabled = enabled
	return s
}

func NewService(repository Repository, contexts ContextResolver, wallets WalletResolver, validator CredentialValidator, providerURLs ProviderURLPolicy, billing BillingClient, upstream UpstreamClient, maximumInputTokens int64) *Service {
	if maximumInputTokens <= 0 {
		maximumInputTokens = 128_000
	}
	service := &Service{repository: repository, contexts: contexts, wallets: wallets, validator: validator, providerURLs: providerURLs, billing: billing, upstream: upstream, maximumInputTokens: maximumInputTokens, defaultMaxOutputTokens: 4096, maximumOutputTokens: 128_000, breaker: resilience.NewBreaker(), operationsOverviewMetrics: newTokenOperationsOverviewMetrics()}
	if resolver, ok := contexts.(CommerceCustomerResolver); ok {
		service.commerceCustomers = resolver
	}
	return service
}

func (s *Service) Handler(serviceToken string, verify identity.VerifyFunc) http.Handler {
	router := httpserver.NewRouter()
	internal := router.Group("/internal/v1")
	internal.Use(serviceauth.GinMiddleware(serviceToken))
	internal.PUT("/organizations/:organization_id/lifecycle", serviceauth.Require("internal.token.organization.lifecycle"), approvalpolicy.Require("TOKEN_ORGANIZATION_LIFECYCLE_APPLY", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.applyOrganizationLifecycle))
	internal.PUT("/organizations/:organization_id/units/:unit_id/lifecycle", serviceauth.Require("internal.token.organization-unit.lifecycle"), approvalpolicy.Require("TOKEN_ORGANIZATION_UNIT_LIFECYCLE_APPLY", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.organizationUnitDeletionBlockers))
	internal.DELETE("/users/:user_id/replay-payloads", serviceauth.Require("internal.token.replay.delete"), httpserver.AdaptFunc(s.anonymizeUserReplayPayloads))
	internal.GET("/users/:user_id/account-closure-assessment", serviceauth.Require("internal.token.account-closure-assessment.read"), httpserver.AdaptFunc(s.accountClosureAssessment))
	internal.GET("/client-readiness", serviceauth.Require("internal.token.client-readiness.read"), httpserver.AdaptFunc(s.clientReadinessStatus))
	internal.POST("/client-readiness/attestations", serviceauth.Require("internal.token.client-readiness.attest"), httpserver.AdaptFunc(s.issueClientReadinessAttestation))
	internal.POST("/commands/model-lifecycle/execute", serviceauth.Require("internal.token.model-lifecycle.execute"), s.requireNewTokenCommandGovernanceActive(), httpserver.AdaptFunc(s.executeApprovedModelLifecycle))
	internal.POST("/commands/catalog-rollback/execute", serviceauth.Require("internal.token.catalog-rollback.execute"), s.requireNewTokenCommandGovernanceActive(), httpserver.AdaptFunc(s.executeApprovedCatalogRollback))
	internal.POST("/commands/credential-revoke/execute", serviceauth.Require("internal.token.credential-revoke.execute"), s.requireNewTokenCommandGovernanceActive(), httpserver.AdaptFunc(s.executeApprovedCredentialRevoke))
	router.GET("/v1/client-compatibility", httpserver.AdaptFunc(s.clientCompatibility))
	protected := router.Group("")
	protected.Use(identity.GinMiddleware(verify))
	protected.GET("/v1/usage-requests/:usage_request_id", httpserver.AdaptFunc(s.getUsageRequest))
	protected.POST("/v1/usage-requests/:usage_request_id/cancel", httpserver.AdaptFunc(s.cancelUsageRequest))
	protected.GET("/v1/me/usage-requests", httpserver.AdaptFunc(s.listMyUsageRequests))
	protected.GET("/v1/model-protocols", httpserver.AdaptFunc(s.modelProtocols))
	protected.GET("/v1/models", httpserver.AdaptFunc(s.listCatalogModels))
	protected.GET("/v1/organizations/:organization_id/model-policy", httpserver.AdaptFunc(s.organizationModelPolicy))
	protected.PUT("/v1/organizations/:organization_id/model-policy", approvalpolicy.Require("TOKEN_ORGANIZATION_MODEL_POLICY_UPDATE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.organizationModelPolicy))
	protected.POST("/v1/chat/completions", httpserver.AdaptFunc(s.chatCompletions))
	protected.GET("/v1/recharge/products", httpserver.AdaptFunc(s.rechargeProducts))
	protected.GET("/v1/payment-capabilities", httpserver.AdaptFunc(s.paymentCapabilities))
	protected.POST("/v1/recharge/orders", httpserver.AdaptFunc(s.createRechargeOrder))
	protected.GET("/v1/organizations/:organization_id/balance", httpserver.AdaptFunc(s.getOrganizationBalance))
	protected.GET("/v1/organizations/:organization_id/entitlement-packages", httpserver.AdaptFunc(s.getOrganizationEntitlementPackages))
	protected.POST("/v1/organizations/:organization_id/recharge/orders", httpserver.AdaptFunc(s.createOrganizationRechargeOrder))
	protected.GET("/v1/organizations/:organization_id/recharge/orders/:order_id", httpserver.AdaptFunc(s.getOrganizationRechargeOrder))
	protected.GET("/v1/organizations/:organization_id/quota-grants", httpserver.AdaptFunc(s.listOrganizationQuotaGrants))
	protected.POST("/v1/organizations/:organization_id/quota-grants:batch", httpserver.AdaptFunc(s.allocateOrganizationQuotaBatch))
	protected.POST("/v1/organizations/:organization_id/quota-grants/:grant_id/revoke", httpserver.AdaptFunc(s.revokeOrganizationQuotaGrant))
	protected.POST("/v1/organizations/:organization_id/account-closed-grants/:grant_id/reclaim", httpserver.AdaptFunc(s.reclaimAccountClosedGrant))
	protected.POST("/v1/organizations/:organization_id/account-closed-grants:reclaim-batch", httpserver.AdaptFunc(s.reclaimAccountClosedGrantsBatch))
	protected.GET("/v1/recharge/orders", httpserver.AdaptFunc(s.listRechargeOrders))
	protected.GET("/v1/recharge/orders/recent", httpserver.AdaptFunc(s.getRecentRechargeOrder))
	protected.GET("/v1/recharge/orders/:order_id", httpserver.AdaptFunc(s.getRechargeOrder))
	protected.POST("/v1/recharge/orders/:order_id/close", httpserver.AdaptFunc(s.closeRechargeOrder))
	protected.POST("/v1/recharge/orders/:order_id/payment-claims", httpserver.AdaptFunc(s.addLatePaymentClaim))
	protected.GET("/v1/me/balance", httpserver.AdaptFunc(s.getBalance))
	protected.GET("/v1/me/entitlement-packages", httpserver.AdaptFunc(s.getEntitlementPackages))
	admin := protected.Group("/admin/v1")
	if s.authorizer != nil {
		admin.GET("/overview", s.authorizer.RequireAny("platform.tokens.read", "platform.usage.read", "platform.system.status.read"), httpserver.AdaptFunc(s.adminOverview))
		admin.GET("/operations-overview", s.authorizer.RequireAnyAnd([]string{"platform.models.read"}, []string{"platform.tokens.read", "platform.usage.read"}), httpserver.AdaptFunc(s.adminTokenOperationsOverview))
		admin.GET("/token-accounts", s.authorizer.Require("platform.tokens.read"), httpserver.AdaptFunc(s.adminTokenAccounts))
		admin.POST("/token-accounts", s.authorizer.Require("platform.tokens.manage"), approvalpolicy.Require("TOKEN_ACCOUNT_CREATE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminCreateTokenAccount))
		admin.GET("/token-accounts/:token_account_id", s.authorizer.Require("platform.tokens.read"), httpserver.AdaptFunc(s.adminTokenAccount))
		admin.PATCH("/token-accounts/:token_account_id/status", s.authorizer.Require("platform.tokens.manage"), approvalpolicy.Require("TOKEN_ACCOUNT_STATUS_CHANGE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminSetTokenAccountStatus))
		admin.PUT("/token-accounts/:token_account_id/routes", s.authorizer.Require("platform.tokens.manage"), s.selectLegacyTokenRouteDirect(), approvalpolicy.Require("TOKEN_ROUTE_UPSERT", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminUpsertTokenRoute))
		admin.DELETE("/token-accounts/:token_account_id/routes", s.authorizer.Require("platform.tokens.manage"), s.selectLegacyTokenRouteDirect(), approvalpolicy.Require("TOKEN_ROUTE_DELETE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminDeleteTokenRoute))
		admin.POST("/token-accounts/:token_account_id/credentials/:credential_id/validate", s.authorizer.Require("platform.tokens.credentials.validate"), approvalpolicy.Require("TOKEN_CREDENTIAL_VALIDATE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminValidateCredential))
		admin.GET("/usage-requests", s.authorizer.Require("platform.usage.read"), httpserver.AdaptFunc(s.adminUsageRequests))
		admin.GET("/usage-requests/:usage_request_id", s.authorizer.Require("platform.usage.read"), httpserver.AdaptFunc(s.adminUsageRequest))
		admin.GET("/usage-requests/:usage_request_id/evidence", s.authorizer.Require("platform.usage.read"), httpserver.AdaptFunc(s.adminUsageEvidence))
		admin.GET("/reconciliation-cases", s.authorizer.Require("platform.usage.read"), httpserver.AdaptFunc(s.adminReconciliationCases))
		admin.POST("/usage-requests/:usage_request_id/retry-reconciliation", s.authorizer.Require("platform.usage.reconcile.retry"), approvalpolicy.Require("TOKEN_USAGE_RECONCILIATION_RETRY", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminRetryReconciliation))
		admin.GET("/protocols", s.authorizer.Require("platform.pricing.read"), httpserver.AdaptFunc(s.adminProtocols))
		admin.GET("/formulas", s.authorizer.Require("platform.pricing.read"), httpserver.AdaptFunc(s.adminFormulas))
		admin.POST("/protocols", s.authorizer.Require("platform.pricing.manage"), approvalpolicy.Require("TOKEN_PROTOCOL_DRAFT_CREATE", approvalpolicy.AuditOnly), s.requireNewTokenCommandGovernanceActive(), httpserver.AdaptFunc(s.adminCreateProtocol))
		admin.POST("/protocols/:protocol_definition_id/publish", s.authorizer.Require("platform.pricing.manage"), approvalpolicy.Require("TOKEN_PROTOCOL_PUBLISH", approvalpolicy.AuditOnly), s.requireNewTokenCommandGovernanceActive(), httpserver.AdaptFunc(s.adminPublishProtocol))
		admin.POST("/protocols/:protocol_definition_id/formulas", s.authorizer.Require("platform.pricing.manage"), approvalpolicy.Require("TOKEN_FORMULA_DRAFT_CREATE", approvalpolicy.AuditOnly), s.requireNewTokenCommandGovernanceActive(), httpserver.AdaptFunc(s.adminCreateFormula))
		admin.POST("/formulas/:formula_version_id/publish", s.authorizer.Require("platform.pricing.manage"), approvalpolicy.Require("TOKEN_FORMULA_PUBLISH", approvalpolicy.AuditOnly), s.requireNewTokenCommandGovernanceActive(), httpserver.AdaptFunc(s.adminPublishFormula))
		admin.GET("/formulas/:formula_version_id/preview", s.authorizer.Require("platform.pricing.read"), httpserver.AdaptFunc(s.adminPreviewFormula))
		admin.GET("/models", s.authorizer.Require("platform.models.read"), httpserver.AdaptFunc(s.adminCatalogModels))
		admin.PUT("/models/:model_id/draft", s.authorizer.Require("platform.models.manage"), approvalpolicy.Require("TOKEN_MODEL_DRAFT_SAVE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminSaveCatalogDraft))
		admin.POST("/models/:model_id/publish", s.authorizer.Require("platform.models.manage"), approvalpolicy.Require("TOKEN_MODEL_PUBLISH", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminPublishCatalogDraft))
		admin.POST("/models/:model_id/lifecycle", s.authorizer.Require("platform.models.manage"), s.selectLegacyTokenDirect(), approvalpolicy.RequireLegacyTokenDirect("TOKEN_MODEL_LIFECYCLE_CHANGE"), httpserver.AdaptFunc(s.adminTransitionCatalogModel))
		admin.POST("/models:sync", s.authorizer.Require("platform.models.sync"), approvalpolicy.Require("TOKEN_MODEL_CATALOG_SYNC", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminSyncCatalog))
		admin.POST("/model-catalog:rollback", s.authorizer.Require("platform.models.manage"), s.selectLegacyTokenDirect(), approvalpolicy.RequireLegacyTokenDirect("TOKEN_MODEL_CATALOG_ROLLBACK"), httpserver.AdaptFunc(s.adminRollbackCatalog))
		admin.GET("/models/:model_id/impact/lifecycle", s.authorizer.Require("platform.models.read"), httpserver.AdaptFunc(s.adminModelLifecycleImpact))
		admin.GET("/model-catalog/impact/rollback", s.authorizer.Require("platform.models.read"), httpserver.AdaptFunc(s.adminCatalogRollbackImpact))
		admin.GET("/provisionings", s.authorizer.Require("platform.tokens.read"), httpserver.AdaptFunc(s.adminProvisionings))
		admin.GET("/provisionings/:provisioning_id", s.authorizer.Require("platform.tokens.read"), httpserver.AdaptFunc(s.adminProvisioning))
		admin.GET("/owner-candidates", s.authorizer.Require("platform.tokens.owner-candidates.read"), httpserver.AdaptFunc(s.adminOwnerCandidates))
		admin.GET("/resource-audit", s.authorizer.RequireAnyAnd([]string{"platform.models.read", "platform.tokens.read"}, []string{"platform.audit.read"}), httpserver.AdaptFunc(s.adminResourceAudit))
		admin.POST("/commands", s.requireNewTokenCommandGovernanceActive(), httpserver.AdaptFunc(s.adminCreateTokenCommand))
		admin.GET("/commands/:command_reference", s.requireNewTokenCommandGovernanceActive(), httpserver.AdaptFunc(s.adminTokenCommandResult))
		admin.GET("/token-accounts/:token_account_id/impact/status", s.authorizer.Require("platform.tokens.read"), httpserver.AdaptFunc(s.adminTokenAccountStatusImpact))
		admin.GET("/token-accounts/:token_account_id/impact/route", s.authorizer.Require("platform.tokens.read"), httpserver.AdaptFunc(s.adminTokenRouteImpact))
		admin.GET("/token-accounts/:token_account_id/impact/credential", s.authorizer.Require("platform.tokens.read"), httpserver.AdaptFunc(s.adminTokenCredentialImpact))
		admin.POST("/provisionings/:provisioning_id/repair", s.authorizer.Require("platform.tokens.manage"), approvalpolicy.Require("TOKEN_ROUTE_PROVISIONING_REPAIR", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminRepairProvisioning))
	}
	adminV2 := protected.Group("/admin/v2")
	if s.authorizer != nil {
		adminV2.PUT("/token-accounts/:token_account_id/routes", s.authorizer.Require("platform.tokens.manage"), s.requireNewTokenCommandGovernanceActive(), approvalpolicy.Require("TOKEN_ROUTE_UPSERT", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminUpsertStableTokenRoute))
		adminV2.DELETE("/token-accounts/:token_account_id/routes", s.authorizer.Require("platform.tokens.manage"), s.requireNewTokenCommandGovernanceActive(), approvalpolicy.Require("TOKEN_ROUTE_DELETE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminDeleteStableTokenRoute))
		adminV2.GET("/token-accounts/:token_account_id/impact/route", s.authorizer.Require("platform.tokens.read"), httpserver.AdaptFunc(s.adminStableTokenRouteImpact))
	}
	return router
}

func (s *Service) anonymizeUserReplayPayloads(w http.ResponseWriter, r *http.Request) {
	userID, err := uuid.Parse(r.PathValue("user_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid user", httpserver.RequestID(r.Context()))
		return
	}
	repository, ok := s.repository.(interface {
		AnonymizeUsageReplay(context.Context, uuid.UUID) error
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "REPLAY_RETENTION_UNAVAILABLE", "replay retention is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if err := repository.AnonymizeUsageReplay(r.Context(), userID); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) modelProtocols(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.resolveModelUsagePermission(w, r, "personal.models.read", "org.models.read"); !ok {
		return
	}
	protocols := s.upstream.Protocols()
	if len(protocols) == 0 {
		api.WriteError(w, http.StatusServiceUnavailable, "MODEL_PROTOCOLS_UNAVAILABLE", "the model gateway adapter declares no supported protocol", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"adapter_version": s.upstream.AdapterVersion(), "protocols": protocols})
}

func (s *Service) listRechargeOrders(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolvePersonalBilling(w, r, "personal.billing.read")
	if !ok {
		return
	}
	walletID, err := s.activeWallet(r.Context(), resolved)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "recharge orders are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	payload, err := s.billing.RechargeOrders(r.Context(), resolved.UserID, walletID)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "recharge orders are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusOK, payload)
}

func (s *Service) rechargeProducts(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.resolveReadContext(w, r); !ok {
		return
	}
	payload, err := s.billing.Products(r.Context())
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "the recharge catalog is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusOK, payload)
}

func (s *Service) paymentCapabilities(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.resolveReadContext(w, r); !ok {
		return
	}
	client, ok := s.billing.(PaymentCapabilitiesBillingClient)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "payment capabilities are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	payload, err := client.PaymentCapabilities(r.Context())
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "payment capabilities are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusOK, payload)
}

func (s *Service) createRechargeOrder(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolvePersonalBilling(w, r, "personal.recharge.create")
	if !ok {
		return
	}
	commerceStatus := "NOT_CONFIGURED"
	if s.commerceCustomers != nil {
		status, err := s.commerceCustomers.ResolveCommerceCustomer(r.Context(), r.Header.Get("Authorization"))
		if err == nil {
			commerceStatus = status
		}
	}
	if !resolved.MobileVerified || resolved.Mobile == nil {
		api.WriteError(w, http.StatusConflict, "MOBILE_NOT_VERIFIED", "a verified mobile is required before recharge", httpserver.RequestID(r.Context()))
		return
	}
	var body struct {
		ProductID string `json:"product_id"`
		Channel   string `json:"channel"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	walletID, err := s.activeWallet(r.Context(), resolved)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "the active wallet is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	payload, err := s.billing.CreateRechargeOrder(paymentSourceContext(r), resolved.UserID, walletID, body.ProductID, strings.ToUpper(body.Channel), *resolved.Mobile, commerceStatus, r.Header.Get("Idempotency-Key"))
	if err != nil {
		var limited *BillingHTTPError
		if errors.As(err, &limited) && limited.Status == http.StatusTooManyRequests {
			if limited.RetryAfter != "" {
				w.Header().Set("Retry-After", limited.RetryAfter)
			}
			api.WriteError(w, http.StatusTooManyRequests, "PAYMENT_RATE_LIMITED", "the payment action exceeds a rate limit", httpserver.RequestID(r.Context()))
			return
		}
		if errors.Is(err, ErrCommerceCustomerNotReady) {
			api.WriteError(w, http.StatusConflict, "COMMERCE_CUSTOMER_NOT_READY", "the payment provider requires a ready Commerce Customer mapping", httpserver.RequestID(r.Context()))
			return
		}
		api.WriteError(w, http.StatusBadGateway, "RECHARGE_CREATE_FAILED", "the recharge order could not be created", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusCreated, payload)
}

func (s *Service) getRechargeOrder(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolvePersonalBilling(w, r, "personal.billing.read")
	if !ok {
		return
	}
	orderID, err := uuid.Parse(r.PathValue("order_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid recharge order", httpserver.RequestID(r.Context()))
		return
	}
	payload, err := s.billing.RechargeOrder(paymentSourceContext(r), orderID, resolved.UserID)
	if err != nil {
		var limited *BillingHTTPError
		if errors.As(err, &limited) && limited.Status == http.StatusTooManyRequests {
			if limited.RetryAfter != "" {
				w.Header().Set("Retry-After", limited.RetryAfter)
			}
			api.WriteError(w, http.StatusTooManyRequests, "PAYMENT_RATE_LIMITED", "the payment action exceeds a rate limit", httpserver.RequestID(r.Context()))
			return
		}
		api.WriteError(w, http.StatusNotFound, "RECHARGE_ORDER_NOT_FOUND", "the recharge order was not found", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusOK, payload)
}

func (s *Service) getRecentRechargeOrder(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolvePersonalBilling(w, r, "personal.billing.read")
	if !ok {
		return
	}
	walletID, err := s.activeWallet(r.Context(), resolved)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "the active wallet is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	payload, err := s.billing.RecentRechargeOrder(r.Context(), resolved.UserID, walletID)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "the recent recharge order is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusOK, payload)
}

func (s *Service) closeRechargeOrder(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolvePersonalBilling(w, r, "personal.recharge.manage")
	if !ok {
		return
	}
	orderID, err := uuid.Parse(r.PathValue("order_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid recharge order", httpserver.RequestID(r.Context()))
		return
	}
	payload, err := s.billing.CloseRechargeOrder(r.Context(), orderID, resolved.UserID)
	if err != nil {
		api.WriteError(w, http.StatusNotFound, "RECHARGE_ORDER_NOT_FOUND", "the recharge order was not found or cannot be closed", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusOK, payload)
}

func (s *Service) addLatePaymentClaim(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolvePersonalBilling(w, r, "personal.recharge.manage")
	if !ok {
		return
	}
	orderID, err := uuid.Parse(r.PathValue("order_id"))
	var body struct {
		Statement string `json:"statement"`
	}
	if decodeErr := api.DecodeJSON(w, r, &body); err != nil || decodeErr != nil || len(strings.TrimSpace(body.Statement)) < 10 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "a valid order and payment statement are required", httpserver.RequestID(r.Context()))
		return
	}
	payload, err := s.billing.AddLatePaymentClaim(r.Context(), orderID, resolved.UserID, body.Statement)
	if err != nil {
		api.WriteError(w, http.StatusConflict, "PAYMENT_CLAIM_REJECTED", "the payment claim cannot be added to this order", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusCreated, payload)
}

func (s *Service) getBalance(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolvePersonalBilling(w, r, "personal.billing.read")
	if !ok {
		return
	}
	walletID, err := s.activeWallet(r.Context(), resolved)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "the active wallet is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	payload, err := s.billing.Balance(r.Context(), walletID)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "the balance is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusOK, payload)
}

func (s *Service) getEntitlementPackages(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolvePersonalBilling(w, r, "personal.billing.read")
	if !ok {
		return
	}
	walletID, err := s.wallets.Ensure(r.Context(), "PERSONAL", resolved.UserID)
	client, supported := s.billing.(EntitlementPackageBillingClient)
	if err != nil || !supported {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "entitlement packages are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	payload, err := client.Packages(r.Context(), resolved.UserID, walletID)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "entitlement packages are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusOK, payload)
}

func (s *Service) activeWallet(ctx context.Context, resolved AuthorizationContext) (uuid.UUID, error) {
	if resolved.ContextType == "PERSONAL" {
		return s.wallets.Ensure(ctx, "PERSONAL", resolved.UserID)
	}
	if resolved.ContextType == "ORGANIZATION" && resolved.OrganizationID != nil {
		return s.wallets.Ensure(ctx, "ORGANIZATION", *resolved.OrganizationID)
	}
	return uuid.Nil, ErrForbidden
}

func writeRawNoStore(w http.ResponseWriter, status int, payload json.RawMessage) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

func gatewaySupportsProtocol(capabilities []GatewayProtocolCapability, protocolCode string) bool {
	for _, capability := range capabilities {
		if capability.ProtocolCode == protocolCode {
			return true
		}
	}
	return false
}

func (s *Service) getUsageRequest(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolveModelUsagePermission(w, r, "personal.usage.read", "org.usage.read.self")
	if !ok {
		return
	}
	usageID, err := uuid.Parse(r.PathValue("usage_request_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid usage request", httpserver.RequestID(r.Context()))
		return
	}
	usage, err := s.repository.GetUsage(r.Context(), usageID)
	if err != nil || !usageContextMatches(resolved, usage) {
		s.writeError(w, r, ErrNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, map[string]any{
		"usage_request_id": usage.ID, "status": usage.Status, "execution_status": usage.ExecutionStatus, "delivery_status": usage.DeliveryStatus, "cancel_status": usage.CancelStatus, "settlement_status": usage.SettlementStatus, "display_status": usageDisplayStatus(usage),
		"model":                usage.Model,
		"maximum_microcredits": usage.MaximumMicrocredits, "actual_microcredits": usage.ActualMicrocredits,
		"cancel_requested_at": usage.CancelRequestedAt, "provider_terminal_at": usage.ProviderTerminalAt, "final_usage_observed_at": usage.FinalUsageObservedAt,
		"last_persisted_sequence": usage.LastPersistedSequence, "last_client_confirmed_sequence": usage.LastClientConfirmedSequence,
		"started_at": usage.StartedAt, "updated_at": usage.UpdatedAt,
	})
}

func (s *Service) cancelUsageRequest(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.authorizePersonalUsageUser(w, r, "personal.usage.cancel")
	if !ok {
		return
	}
	usageID, err := uuid.Parse(r.PathValue("usage_request_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid usage request", httpserver.RequestID(r.Context()))
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if len(key) < 16 || len(key) > 128 {
		api.WriteError(w, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key must contain 16 to 128 characters", httpserver.RequestID(r.Context()))
		return
	}
	result, preDispatch, err := s.repository.RequestCancellation(r.Context(), usageID, userID, digestString([]byte(key)), cancellationFingerprint(usageID), s.upstream.AdapterVersion())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if preDispatch {
		result, err = s.finishPreDispatchCancellation(context.WithoutCancel(r.Context()), usageID)
		if err != nil {
			if definitiveBillingFailure(err) {
				s.writeError(w, r, err)
				return
			}
			w.Header().Set("Location", "/v1/usage-requests/"+usageID.String())
			api.WriteJSON(w, http.StatusAccepted, result)
			return
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) finishPreDispatchCancellation(ctx context.Context, usageID uuid.UUID) (CancellationResult, error) {
	usage, err := s.repository.GetUsage(ctx, usageID)
	if err != nil {
		return CancellationResult{}, err
	}
	if usage.ExecutionStatus == "RECEIVED" {
		return s.repository.ConfirmPreDispatchCancellation(ctx, usageID)
	}
	if usage.HoldID == nil {
		envelope, err := s.repository.LoadDispatchPreparation(ctx, usageID)
		if err != nil {
			return CancellationResult{}, err
		}
		hold, err := s.ensureUsageHold(ctx, usage, envelope)
		if err != nil {
			if definitiveBillingFailure(err) {
				return s.repository.ConfirmPreDispatchCancellation(ctx, usageID)
			}
			return cancellationSnapshot(usage), err
		}
		err = s.repository.CompleteHoldAndDispatch(ctx, usageID, hold.ID, envelope.Price.ID)
		if err != nil && !errors.Is(err, ErrCancellationRequested) {
			return CancellationResult{}, err
		}
		usage, err = s.repository.GetUsage(ctx, usageID)
		if err != nil {
			return CancellationResult{}, err
		}
	}
	if usage.ExecutionStatus != "HOLD_RESERVING" && usage.ExecutionStatus != "HOLD_RESERVED" {
		return cancellationSnapshot(usage), nil
	}
	if usage.HoldID != nil {
		if err := s.billing.Release(ctx, usageID, *usage.HoldID, "cancelled before dispatch intent"); err != nil {
			return cancellationSnapshot(usage), err
		}
	}
	return s.repository.ConfirmPreDispatchCancellation(ctx, usageID)
}

func (s *Service) listMyUsageRequests(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolveModelUsagePermission(w, r, "personal.usage.read", "org.usage.read.self")
	if !ok {
		return
	}
	limit := 20
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "limit must be between 1 and 100", httpserver.RequestID(r.Context()))
			return
		}
		limit = parsed
	}
	var cursor *UsageHistoryCursor
	if raw := strings.TrimSpace(r.URL.Query().Get("cursor")); raw != "" {
		parsed, err := decodeUsageCursor(raw)
		if err != nil {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "cursor is invalid", httpserver.RequestID(r.Context()))
			return
		}
		cursor = &parsed
	}
	usageRequests, err := s.repository.ListUsageForContext(r.Context(), resolved, limit+1, cursor)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var nextCursor *string
	if len(usageRequests) > limit {
		last := usageRequests[limit-1]
		encoded := encodeUsageCursor(UsageHistoryCursor{StartedAt: last.StartedAt, ID: last.UsageRequestID})
		nextCursor = &encoded
		usageRequests = usageRequests[:limit]
	}
	for index := range usageRequests {
		usageRequests[index].DisplayStatus = usageDisplayStatus(UsageRequest{Status: usageRequests[index].status, ExecutionStatus: usageRequests[index].ExecutionStatus, DeliveryStatus: usageRequests[index].DeliveryStatus, CancelStatus: usageRequests[index].CancelStatus, SettlementStatus: usageRequests[index].SettlementStatus})
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": usageRequests, "next_cursor": nextCursor})
}

func encodeUsageCursor(cursor UsageHistoryCursor) string {
	payload := cursor.StartedAt.UTC().Format(time.RFC3339Nano) + "|" + cursor.ID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(payload))
}

func decodeUsageCursor(raw string) (UsageHistoryCursor, error) {
	if len(raw) > 256 {
		return UsageHistoryCursor{}, ErrForbidden
	}
	payload, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return UsageHistoryCursor{}, err
	}
	parts := strings.SplitN(string(payload), "|", 2)
	if len(parts) != 2 {
		return UsageHistoryCursor{}, ErrForbidden
	}
	startedAt, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return UsageHistoryCursor{}, err
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return UsageHistoryCursor{}, err
	}
	return UsageHistoryCursor{StartedAt: startedAt, ID: id}, nil
}

func usageContextMatches(context AuthorizationContext, usage UsageRequest) bool {
	if usage.UserID != context.UserID || usage.SessionID != context.SessionID {
		return false
	}
	if context.ContextType == "PERSONAL" {
		return usage.OrganizationID == nil
	}
	return context.ContextType == "ORGANIZATION" && context.OrganizationID != nil && usage.OrganizationID != nil && *context.OrganizationID == *usage.OrganizationID
}

func usageDisplayStatus(usage UsageRequest) string {
	if usage.DeliveryStatus == "USER_STOPPED" && usage.CancelStatus != "LOCAL_CONFIRMED" {
		return "DISPLAY_STOPPED_EXECUTION_CONTINUES"
	}
	if usage.CancelStatus == "LOCAL_CONFIRMED" {
		return "CANCELLED_BEFORE_DISPATCH"
	}
	switch usage.Status {
	case "CREATED", "RESERVED", "UPSTREAM_ACTIVE":
		return "IN_PROGRESS"
	case "COMMITTED":
		return "SETTLED"
	case "SETTLEMENT_PENDING":
		return "SETTLEMENT_PENDING"
	case "RELEASED":
		return "RELEASED"
	default:
		return "MANUAL_REVIEW"
	}
}

func (s *Service) resolveContext(w http.ResponseWriter, r *http.Request) (AuthorizationContext, bool) {
	value := r.Header.Get("Authorization")
	resolved, err := s.contexts.Resolve(r.Context(), value)
	if err != nil {
		s.writeError(w, r, err)
		return AuthorizationContext{}, false
	}
	w.Header().Set("X-Context-Version", strconv.FormatInt(max(resolved.ContextVersion, 1), 10))
	w.Header().Set("X-Authorization-Version", strconv.FormatInt(max(resolved.AuthorizationVersion, 1), 10))
	return resolved, true
}

func (s *Service) resolveReadContext(w http.ResponseWriter, r *http.Request) (AuthorizationContext, bool) {
	value := r.Header.Get("Authorization")
	resolved, err := s.contexts.Resolve(r.Context(), value)
	if errors.Is(err, ErrAccountClosing) {
		reader, ok := s.contexts.(interface {
			ResolveClosureRead(context.Context, string) (AuthorizationContext, error)
		})
		if ok {
			resolved, err = reader.ResolveClosureRead(r.Context(), value)
		}
	}
	if err != nil {
		s.writeError(w, r, err)
		return AuthorizationContext{}, false
	}
	w.Header().Set("X-Context-Version", strconv.FormatInt(max(resolved.ContextVersion, 1), 10))
	w.Header().Set("X-Authorization-Version", strconv.FormatInt(max(resolved.AuthorizationVersion, 1), 10))
	return resolved, true
}

func (s *Service) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := http.StatusInternalServerError, "INTERNAL_ERROR", "the token request could not be completed"
	switch {
	case errors.Is(err, ErrRateLimited):
		retryAfter := 1
		var limited *RateLimitError
		if errors.As(err, &limited) {
			retryAfter = max(1, int(limited.RetryAfter.Seconds()+0.999))
		}
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		api.WriteJSON(w, http.StatusTooManyRequests, api.Error{Code: "MODEL_RATE_LIMITED", Message: "the model request exceeds a rate or concurrency limit", RequestID: httpserver.RequestID(r.Context()), Details: map[string]any{"retry_after_seconds": retryAfter}})
		return
	case errors.Is(err, ErrNotFound):
		status, code, message = http.StatusNotFound, "TOKEN_ACCOUNT_NOT_FOUND", "the token account was not found"
	case errors.Is(err, ErrForbidden):
		status, code, message = http.StatusForbidden, "TOKEN_FORBIDDEN", "the token action is not permitted"
	case errors.Is(err, ErrAmbiguousRoute):
		status, code, message = http.StatusConflict, "TOKEN_ROUTE_AMBIGUOUS", "multiple token routes have the same priority"
	case errors.Is(err, ErrRouteVersionConflict):
		status, code, message = http.StatusConflict, "TOKEN_ROUTE_VERSION_CONFLICT", "the token account or route version has changed"
	case errors.Is(err, ErrRouteReferenceInvalid):
		status, code, message = http.StatusUnprocessableEntity, "TOKEN_ROUTE_REFERENCE_INVALID", "the published model or protocol reference is unavailable"
	case errors.Is(err, ErrTokenRouteActivationChanged):
		status, code, message = http.StatusServiceUnavailable, "TOKEN_COMMAND_GOVERNANCE_CHANGED", "the token route governance state changed; retry against the selected contract"
	case errors.Is(err, ErrNoEligibleRoute):
		status, code, message = http.StatusUnprocessableEntity, "TOKEN_ROUTE_UNAVAILABLE", "no eligible token route is available"
	case errors.Is(err, ErrAccountProvisioning):
		status, code, message = http.StatusConflict, "ACCOUNT_PROVISIONING", "the account is still provisioning"
	case errors.Is(err, ErrAccountRepairRequired):
		status, code, message = http.StatusConflict, "ACCOUNT_REPAIR_REQUIRED", "the account initialization requires repair"
	case errors.Is(err, ErrAccountClosing):
		status, code, message = http.StatusConflict, "ACCOUNT_CLOSING", "the account is in its closure quiet period"
	case errors.Is(err, ErrOrganizationLifecycleConflict):
		status, code, message = http.StatusConflict, "ORGANIZATION_LIFECYCLE_CONFLICT", "the organization lifecycle version or state conflicts with current facts"
	case errors.Is(err, ErrCatalogVersionConflict):
		status, code, message = http.StatusConflict, "MODEL_CATALOG_VERSION_CONFLICT", "the model catalog or draft version has changed"
	case errors.Is(err, ErrCatalogAliasConflict):
		status, code, message = http.StatusConflict, "MODEL_ALIAS_CONFLICT", "the model name or alias is ambiguous"
	case errors.Is(err, ErrIdempotencyConflict):
		status, code, message = http.StatusConflict, "IDEMPOTENCY_CONFLICT", "the idempotency key was already used with different input"
	case errors.Is(err, ErrModelDeprecated):
		status, code, message = http.StatusGone, "MODEL_DEPRECATION_CUTOFF_REACHED", "the model can no longer be used for this conversation mode"
	case errors.Is(err, ErrModelUnavailable):
		status, code, message = http.StatusUnprocessableEntity, "MODEL_UNAVAILABLE", "the catalog model is not available for new requests"
	case errors.Is(err, ErrModelPolicyDenied):
		status, code, message = http.StatusForbidden, "MODEL_POLICY_DENIED", "the current organization policy does not allow this model"
	case errors.Is(err, ErrModelPolicyVersionConflict):
		status, code, message = http.StatusConflict, "MODEL_POLICY_VERSION_CONFLICT", "the organization model policy has changed"
	case errors.Is(err, ErrRouteProvisioningPending):
		status, code, message = http.StatusConflict, "ROUTE_PROVISIONING_PENDING", "the managed model route is being prepared"
	case errors.Is(err, ErrRouteProvisioningFailed):
		status, code, message = http.StatusServiceUnavailable, "ROUTE_PROVISIONING_FAILED", "the managed model route is temporarily unavailable"
	case errors.Is(err, ErrRouteRepairRequired):
		status, code, message = http.StatusServiceUnavailable, "ROUTE_REPAIR_REQUIRED", "the managed model route requires operator repair"
	}
	api.WriteError(w, status, code, message, httpserver.RequestID(r.Context()))
}

type HTTPContextResolver struct {
	baseURL *url.URL
	client  *http.Client
}

func NewHTTPContextResolver(rawURL string) (*HTTPContextResolver, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("ACCOUNT_HTTP_URL must be an absolute URL")
	}
	return &HTTPContextResolver{baseURL: parsed, client: observability.NewHTTPClient(5 * time.Second)}, nil
}

func (r *HTTPContextResolver) Resolve(ctx context.Context, authorization string) (AuthorizationContext, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL.String()+"/v1/me/authorization-context", nil)
	if err != nil {
		return AuthorizationContext{}, err
	}
	request.Header.Set("Authorization", authorization)
	response, err := r.client.Do(request)
	if err != nil {
		return AuthorizationContext{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		var problem struct {
			Code string `json:"code"`
		}
		_ = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&problem)
		if problem.Code == "ACCOUNT_PROVISIONING" {
			return AuthorizationContext{}, ErrAccountProvisioning
		}
		if problem.Code == "ACCOUNT_REPAIR_REQUIRED" {
			return AuthorizationContext{}, ErrAccountRepairRequired
		}
		if problem.Code == "ACCOUNT_CLOSING" {
			return AuthorizationContext{}, ErrAccountClosing
		}
		return AuthorizationContext{}, ErrForbidden
	}
	var body struct {
		UserID                    uuid.UUID                  `json:"user_id"`
		SessionID                 string                     `json:"session_id"`
		ContextType               string                     `json:"context_type"`
		OrganizationID            *uuid.UUID                 `json:"organization_id"`
		MembershipVersion         int64                      `json:"membership_version"`
		ContextVersion            int64                      `json:"context_version"`
		AuthorizationVersion      int64                      `json:"authorization_version"`
		UnitIDs                   []uuid.UUID                `json:"unit_ids"`
		AncestorUnitIDs           []uuid.UUID                `json:"ancestor_unit_ids"`
		Roles                     []string                   `json:"roles"`
		Permissions               []string                   `json:"permissions"`
		PersonalRoles             []string                   `json:"personal_roles"`
		PersonalPermissions       []string                   `json:"personal_permissions"`
		UnitAuthorizationBindings []UnitAuthorizationBinding `json:"unit_authorization_bindings"`
		Mobile                    *string                    `json:"mobile"`
		MobileVerified            bool                       `json:"mobile_verified"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&body); err != nil {
		return AuthorizationContext{}, err
	}
	roles, permissions := activeScopeAuthorization(body.ContextType, body.Roles, body.Permissions, body.PersonalRoles, body.PersonalPermissions)
	return AuthorizationContext{UserID: body.UserID, SessionID: body.SessionID, ContextType: body.ContextType, OrganizationID: body.OrganizationID, MembershipVersion: body.MembershipVersion, ContextVersion: body.ContextVersion, AuthorizationVersion: body.AuthorizationVersion, UnitIDs: body.UnitIDs, AncestorUnitIDs: body.AncestorUnitIDs, Roles: roles, Permissions: permissions, UnitAuthorizationBindings: body.UnitAuthorizationBindings, Mobile: body.Mobile, MobileVerified: body.MobileVerified}, nil
}

func activeScopeAuthorization(contextType string, roles, permissions, personalRoles, personalPermissions []string) ([]string, []string) {
	if contextType != "PERSONAL" {
		return roles, permissions
	}
	// Accept both the current three-scope Account contract and the earlier
	// active-scope-only payload during a rolling deployment.
	if personalRoles != nil {
		roles = personalRoles
	}
	if personalPermissions != nil {
		permissions = personalPermissions
	}
	return roles, permissions
}

func (r *HTTPContextResolver) ResolveClosureRead(ctx context.Context, authorization string) (AuthorizationContext, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL.String()+"/v1/me", nil)
	if err != nil {
		return AuthorizationContext{}, err
	}
	request.Header.Set("Authorization", authorization)
	response, err := r.client.Do(request)
	if err != nil {
		return AuthorizationContext{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return AuthorizationContext{}, ErrForbidden
	}
	var body struct {
		UserID        uuid.UUID `json:"user_id"`
		Status        string    `json:"status"`
		ActiveContext struct {
			Type                 string     `json:"context_type"`
			OrganizationID       *uuid.UUID `json:"organization_id"`
			MembershipVersion    int64      `json:"membership_version"`
			ContextVersion       int64      `json:"context_version"`
			AuthorizationVersion int64      `json:"authorization_version"`
		} `json:"active_context"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&body); err != nil || body.UserID == uuid.Nil || body.Status != "CLOSING" {
		return AuthorizationContext{}, ErrForbidden
	}
	return AuthorizationContext{UserID: body.UserID, ContextType: body.ActiveContext.Type, OrganizationID: body.ActiveContext.OrganizationID, MembershipVersion: body.ActiveContext.MembershipVersion, ContextVersion: body.ActiveContext.ContextVersion, AuthorizationVersion: body.ActiveContext.AuthorizationVersion, Closing: true}, nil
}

func (r *HTTPContextResolver) ResolveCommerceCustomer(ctx context.Context, authorization string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL.String()+"/v1/me/commerce-customer-mapping", nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", authorization)
	response, err := r.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", ErrForbidden
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&body); err != nil {
		return "", err
	}
	return body.Status, nil
}

type HTTPWalletResolver struct {
	baseURL      *url.URL
	serviceToken string
	client       *http.Client
}

func NewHTTPWalletResolver(rawURL, serviceToken string) (*HTTPWalletResolver, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || strings.TrimSpace(serviceToken) == "" {
		return nil, errors.New("invalid Billing wallet resolver configuration")
	}
	return &HTTPWalletResolver{baseURL: parsed, serviceToken: serviceToken, client: observability.NewHTTPClient(5 * time.Second)}, nil
}

func (r *HTTPWalletResolver) Ensure(ctx context.Context, ownerType string, ownerID uuid.UUID) (uuid.UUID, error) {
	payload, _ := json.Marshal(map[string]any{"owner_type": ownerType, "owner_id": ownerID})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL.String()+"/internal/v1/wallets/ensure", bytes.NewReader(payload))
	if err != nil {
		return uuid.Nil, err
	}
	if err := serviceauth.SetAuthorization(request, r.serviceToken); err != nil {
		return uuid.Nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := r.client.Do(request)
	if err != nil {
		return uuid.Nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return uuid.Nil, fmt.Errorf("Billing wallet provisioning returned HTTP %d", response.StatusCode)
	}
	var body struct {
		WalletID uuid.UUID `json:"wallet_id"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&body); err != nil || body.WalletID == uuid.Nil {
		return uuid.Nil, errors.New("Billing returned an invalid wallet")
	}
	return body.WalletID, nil
}

type ProviderURLPolicy struct {
	environment  string
	allowedHosts map[string]bool
}

func NewProviderURLPolicy(environment string, allowedHosts []string) (ProviderURLPolicy, error) {
	policy := ProviderURLPolicy{environment: strings.ToLower(strings.TrimSpace(environment)), allowedHosts: map[string]bool{}}
	for _, host := range allowedHosts {
		host = strings.ToLower(strings.TrimSpace(host))
		if host != "" {
			policy.allowedHosts[host] = true
		}
	}
	if len(policy.allowedHosts) == 0 {
		return ProviderURLPolicy{}, errors.New("TOKEN_PROVIDER_ALLOWED_HOSTS must not be empty")
	}
	return policy, nil
}

func (p ProviderURLPolicy) Validate(rawURL string) error {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || parsed.Scheme != "https" && parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || !p.allowedHosts[strings.ToLower(parsed.Hostname())] {
		return ErrInvalidSecret
	}
	if (p.environment == "production" || p.environment == "production-new") && parsed.Scheme != "https" {
		return ErrInvalidSecret
	}
	return nil
}

type HTTPModelValidator struct {
	client *http.Client
	policy ProviderURLPolicy
}

func NewHTTPModelValidator(policy ProviderURLPolicy) *HTTPModelValidator {
	return &HTTPModelValidator{client: observability.NewHTTPClient(10 * time.Second), policy: policy}
}

func (v *HTTPModelValidator) Validate(ctx context.Context, rawURL string, secret []byte, models []string) error {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || v.policy.Validate(rawURL) != nil || len(secret) == 0 || len(normalizeModels(models)) == 0 {
		return ErrInvalidSecret
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String()+"/v1/models", nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+string(secret))
	response, err := v.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ErrInvalidSecret
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&payload); err != nil {
		return err
	}
	available := map[string]bool{}
	for _, item := range payload.Data {
		available[item.ID] = true
	}
	for _, model := range normalizeModels(models) {
		if !available[model] {
			return ErrInvalidSecret
		}
	}
	return nil
}
