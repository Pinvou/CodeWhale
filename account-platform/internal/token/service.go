package token

import (
	"bytes"
	"context"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"

	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"sync/atomic"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/resilience"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"

	"github.com/google/uuid"
)

type ContextResolver interface {
	Resolve(context.Context, string) (AuthorizationContext, error)
}

type WalletResolver interface {
	Ensure(context.Context, string, uuid.UUID) (uuid.UUID, error)
}

func (s *Service) WithAuthorizer(authorizer *authorization.Client) *Service {
	s.authorizer = authorizer
	return s
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
	if err == nil {
		_, err = validateWalletFacts(payload, walletID, resolved.UserID, true)
	}
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "the balance is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusOK, payload)
}

func writeRawNoStore(w http.ResponseWriter, status int, payload json.RawMessage) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
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

type HTTPWalletResolver struct {
	baseURL      *url.URL
	serviceToken string
	client       *http.Client
}

func NewHTTPWalletResolver(rawURL, serviceToken string) (*HTTPWalletResolver, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.TrimSpace(serviceToken) == "" {
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
	var body walletFacts
	if err := decodeAccountAuthority(response.Body, 1<<20, &body); err != nil || ownerID == uuid.Nil || !body.validIdentity() || body.OwnerType != ownerType || body.OwnerID != ownerID {
		return uuid.Nil, errors.New("Billing returned an invalid wallet")
	}
	return body.WalletID, nil
}

// Personal finance exposes only the completed payment, wallet and entitlement
// routes. Model calls and organization operations have separate owners.
type Service struct {
	executionUpstream        UpstreamClient
	executionBilling         ModelExecutionBillingClient
	clientReadiness          *ClientReadinessService
	maximumInputTokens       int64
	defaultMaxOutputTokens   int64
	maximumOutputTokens      int64
	breaker                  *resilience.Breaker
	personalExecutionEnabled bool
	contexts                 ContextResolver
	wallets                  WalletResolver
	billing                  BillingClient
	commerceCustomers        CommerceCustomerResolver
	authorizer               *authorization.Client
	managementVerify         identity.VerifyFunc
	managementEnabled        bool
	managementServiceToken   string
	repository               Repository
	upstream                 ManagementCatalogAdapter
	cancellationStore        managementCancellationStore
	cancellationBilling      cancellationBillingClient
	cancellationAdapter      interface {
		CancelCapability() ProviderCancelCapability
	}
	activeUpstreamCancels       sync.Map
	walletProvisioning          WalletProvisioningClient
	ownerAuthority              OwnerAuthorityClient
	validator                   CredentialValidator
	providerURLs                ProviderURLPolicy
	defaultRoutes               DefaultRouteProvisioner
	catalogSync                 *CatalogSynchronizer
	ownerCandidates             OwnerCandidateDirectory
	commandGovernanceActivation TokenCommandGovernanceActivationReader
	impactPrechecksEnabled      bool
	impactPrechecks             ImpactPrecheckRepository
	impactPricing               ImpactPriceCoverageClient
	approvedCommandExecutor     ApprovedTokenCommandExecutor
	approvalEffects             ApprovalEffectReader
	operationsOverviewEnabled   bool
	operationsOverviewMetrics   *tokenOperationsOverviewMetrics
	pageOverviewsEnabled        bool
	credentialIsolationEnabled  bool
	tokenAccountCreateReady     atomic.Bool
	adminProxyHMACKey           []byte
}

type BillingClient interface {
	Balance(context.Context, uuid.UUID) (json.RawMessage, error)
	Products(context.Context) (json.RawMessage, error)
	CreateRechargeOrder(context.Context, uuid.UUID, uuid.UUID, string, string, string, string, string) (json.RawMessage, error)
	RechargeOrders(context.Context, uuid.UUID, uuid.UUID) (json.RawMessage, error)
	RecentRechargeOrder(context.Context, uuid.UUID, uuid.UUID) (json.RawMessage, error)
	RechargeOrder(context.Context, uuid.UUID, uuid.UUID) (json.RawMessage, error)
	CloseRechargeOrder(context.Context, uuid.UUID, uuid.UUID) (json.RawMessage, error)
	AddLatePaymentClaim(context.Context, uuid.UUID, uuid.UUID, string) (json.RawMessage, error)
}

func NewService(contexts ContextResolver, wallets WalletResolver, billing BillingClient) *Service {
	service := &Service{contexts: contexts, wallets: wallets, billing: billing}
	if resolver, ok := contexts.(CommerceCustomerResolver); ok {
		service.commerceCustomers = resolver
	}
	return service
}

func (s *Service) Handler(verify identity.VerifyFunc) http.Handler {
	router := httpserver.NewRouter()
	protected := router.Group("/v1")
	protected.Use(identity.GinMiddleware(verify))
	protected.GET("/me/balance", httpserver.AdaptFunc(s.getBalance))
	protected.GET("/recharge/products", httpserver.AdaptFunc(s.rechargeProducts))
	protected.GET("/payment-capabilities", httpserver.AdaptFunc(s.paymentCapabilities))
	protected.POST("/recharge/orders", httpserver.AdaptFunc(s.createRechargeOrder))
	protected.GET("/recharge/orders", httpserver.AdaptFunc(s.listRechargeOrders))
	protected.GET("/recharge/orders/recent", httpserver.AdaptFunc(s.getRecentRechargeOrder))
	protected.GET("/recharge/orders/:order_id", httpserver.AdaptFunc(s.getRechargeOrder))
	protected.POST("/recharge/orders/:order_id/close", httpserver.AdaptFunc(s.closeRechargeOrder))
	protected.POST("/recharge/orders/:order_id/payment-claims", httpserver.AdaptFunc(s.addLatePaymentClaim))
	protected.GET("/me/entitlement-packages", httpserver.AdaptFunc(s.getEntitlementPackages))
	s.registerOrganizationFinanceRoutes(protected)
	s.registerPersonalExecutionRoutes(router, protected)
	s.registerManagementRoutes(router, protected)
	return TokenCredentialDeadlineMiddleware(router)
}

func (s *Service) activeWallet(ctx context.Context, resolved AuthorizationContext) (uuid.UUID, error) {
	if resolved.UserID == uuid.Nil {
		return uuid.Nil, ErrForbidden
	}
	if resolved.ContextType == "PERSONAL" && resolved.OrganizationID == nil {
		return s.wallets.Ensure(ctx, "PERSONAL", resolved.UserID)
	}
	if resolved.ContextType == "ORGANIZATION" && resolved.OrganizationID != nil && *resolved.OrganizationID != uuid.Nil {
		return s.wallets.Ensure(ctx, "ORGANIZATION", *resolved.OrganizationID)
	}
	return uuid.Nil, ErrForbidden
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
		status, code, message = http.StatusNotFound, "TOKEN_NOT_FOUND", "the token resource was not found"
	case errors.Is(err, ErrNoEligibleRoute):
		status, code, message = http.StatusUnprocessableEntity, "TOKEN_ROUTE_UNAVAILABLE", "no eligible token route is available"
	case errors.Is(err, ErrIdempotencyConflict):
		status, code, message = http.StatusConflict, "IDEMPOTENCY_CONFLICT", "the idempotency key was already used with different input"
	case errors.Is(err, ErrForbidden):
		status, code, message = http.StatusForbidden, "TOKEN_FORBIDDEN", "the token action is not permitted"
	case errors.Is(err, ErrAccountProvisioning):
		status, code, message = http.StatusConflict, "ACCOUNT_PROVISIONING", "the account is still provisioning"
	case errors.Is(err, ErrAccountRepairRequired):
		status, code, message = http.StatusConflict, "ACCOUNT_REPAIR_REQUIRED", "the account initialization requires repair"
	case errors.Is(err, ErrAccountAuthorityUnavailable):
		status, code, message = http.StatusServiceUnavailable, "ACCOUNT_UNAVAILABLE", "the account context is temporarily unavailable"
	}
	api.WriteError(w, status, code, message, httpserver.RequestID(r.Context()))
}
