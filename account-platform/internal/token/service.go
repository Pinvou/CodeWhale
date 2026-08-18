package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/google/uuid"
)

type ContextResolver interface {
	Resolve(context.Context, string) (AuthorizationContext, error)
}

type WalletResolver interface {
	Ensure(context.Context, string, uuid.UUID) (uuid.UUID, error)
}

type CredentialValidator interface {
	Validate(context.Context, string, []byte, []string) error
}

type Service struct {
	repository             Repository
	contexts               ContextResolver
	wallets                WalletResolver
	validator              CredentialValidator
	providerURLs           ProviderURLPolicy
	billing                BillingClient
	upstream               UpstreamClient
	maximumInputTokens     int64
	defaultMaxOutputTokens int64
	maximumOutputTokens    int64
}

func NewService(repository Repository, contexts ContextResolver, wallets WalletResolver, validator CredentialValidator, providerURLs ProviderURLPolicy, billing BillingClient, upstream UpstreamClient, maximumInputTokens int64) *Service {
	if maximumInputTokens <= 0 {
		maximumInputTokens = 128_000
	}
	return &Service{repository: repository, contexts: contexts, wallets: wallets, validator: validator, providerURLs: providerURLs, billing: billing, upstream: upstream, maximumInputTokens: maximumInputTokens, defaultMaxOutputTokens: 4096, maximumOutputTokens: 128_000}
}

func (s *Service) Handler(verify identity.VerifyFunc) http.Handler {
	router := httpserver.NewRouter()
	router.Use(identity.GinMiddleware(verify))
	router.GET("/v1/token-accounts", httpserver.AdaptFunc(s.listManagedTokenAccounts))
	router.POST("/v1/token-accounts", httpserver.AdaptFunc(s.createTokenAccount))
	router.PATCH("/v1/token-accounts/:token_account_id/status", httpserver.AdaptFunc(s.setTokenAccountStatus))
	router.PUT("/v1/token-accounts/:token_account_id/sharing", httpserver.AdaptFunc(s.updateTokenSharing))
	router.PUT("/v1/token-accounts/:token_account_id/routes", httpserver.AdaptFunc(s.upsertTokenRoute))
	router.POST("/v1/token-accounts/:token_account_id/credentials", httpserver.AdaptFunc(s.rotateCredential))
	router.GET("/v1/token-accounts/:token_account_id/credentials", httpserver.AdaptFunc(s.listCredentials))
	router.POST("/v1/token-accounts/:token_account_id/credentials/:credential_id/revoke", httpserver.AdaptFunc(s.revokeCredential))
	router.GET("/v1/token-accounts/eligible", httpserver.AdaptFunc(s.listEligible))
	router.GET("/v1/usage-requests/:usage_request_id", httpserver.AdaptFunc(s.getUsageRequest))
	router.GET("/v1/me/usage-requests", httpserver.AdaptFunc(s.listMyUsageRequests))
	router.POST("/v1/chat/completions", httpserver.AdaptFunc(s.chatCompletions))
	router.GET("/v1/recharge/products", httpserver.AdaptFunc(s.rechargeProducts))
	router.POST("/v1/recharge/orders", httpserver.AdaptFunc(s.createRechargeOrder))
	router.GET("/v1/recharge/orders/recent", httpserver.AdaptFunc(s.getRecentRechargeOrder))
	router.GET("/v1/recharge/orders/:order_id", httpserver.AdaptFunc(s.getRechargeOrder))
	router.POST("/v1/recharge/orders/:order_id/close", httpserver.AdaptFunc(s.closeRechargeOrder))
	router.GET("/v1/me/balance", httpserver.AdaptFunc(s.getBalance))
	admin := router.Group("/admin/v1")
	admin.Use(identity.RequireRealmRoleGin("platform-admin"))
	admin.GET("/overview", httpserver.AdaptFunc(s.adminOverview))
	admin.GET("/token-accounts", httpserver.AdaptFunc(s.adminTokenAccounts))
	admin.GET("/usage-requests", httpserver.AdaptFunc(s.adminUsageRequests))
	return router
}

func (s *Service) listManagedTokenAccounts(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolveContext(w, r)
	if !ok {
		return
	}
	items, err := s.repository.ListManagedAccounts(r.Context(), resolved)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) listCredentials(w http.ResponseWriter, r *http.Request) {
	_, account, ok := s.resolveOwnedAccount(w, r)
	if !ok {
		return
	}
	items, err := s.repository.ListCredentials(r.Context(), account.ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) setTokenAccountStatus(w http.ResponseWriter, r *http.Request) {
	resolved, account, ok := s.resolveOwnedAccount(w, r)
	if !ok {
		return
	}
	var body struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	if strings.TrimSpace(body.Reason) == "" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "a status change reason is required", httpserver.RequestID(r.Context()))
		return
	}
	updated, err := s.repository.SetAccountStatus(r.Context(), account.ID, body.Status, resolved.UserID, body.Reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, updated)
}

func (s *Service) updateTokenSharing(w http.ResponseWriter, r *http.Request) {
	resolved, account, ok := s.resolveOwnedAccount(w, r)
	if !ok {
		return
	}
	var body struct {
		ShareScope    string      `json:"share_scope"`
		ExplicitUsers []uuid.UUID `json:"explicit_user_ids"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	updated, err := s.repository.UpdateSharePolicy(r.Context(), account.ID, body.ShareScope, body.ExplicitUsers, resolved.UserID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, updated)
}

func (s *Service) upsertTokenRoute(w http.ResponseWriter, r *http.Request) {
	resolved, account, ok := s.resolveOwnedAccount(w, r)
	if !ok {
		return
	}
	var body struct {
		Model        string `json:"model"`
		Priority     int    `json:"priority"`
		DefaultScope string `json:"default_scope"`
		HealthStatus string `json:"health_status"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	policy, err := s.repository.UpsertRoute(r.Context(), account.ID, body.Model, body.Priority, body.DefaultScope, body.HealthStatus, resolved.UserID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, policy)
}

func (s *Service) revokeCredential(w http.ResponseWriter, r *http.Request) {
	resolved, account, ok := s.resolveOwnedAccount(w, r)
	if !ok {
		return
	}
	credentialID, err := uuid.Parse(r.PathValue("credential_id"))
	var body struct {
		Reason string `json:"reason"`
	}
	if decodeErr := api.DecodeJSON(w, r, &body); err != nil || decodeErr != nil || strings.TrimSpace(body.Reason) == "" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid credential revocation", httpserver.RequestID(r.Context()))
		return
	}
	credential, err := s.repository.RevokeCredential(r.Context(), account.ID, credentialID, resolved.UserID, body.Reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, credential)
}

func (s *Service) resolveOwnedAccount(w http.ResponseWriter, r *http.Request) (AuthorizationContext, TokenAccount, bool) {
	resolved, ok := s.resolveContext(w, r)
	if !ok {
		return AuthorizationContext{}, TokenAccount{}, false
	}
	accountID, err := uuid.Parse(r.PathValue("token_account_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid token account", httpserver.RequestID(r.Context()))
		return AuthorizationContext{}, TokenAccount{}, false
	}
	account, err := s.repository.GetAccount(r.Context(), accountID)
	if err != nil || !authorizeOwner(resolved, account.OwnerType, account.OwnerID, account.OwnerOrganization, account.OwnerUnit) {
		s.writeError(w, r, ErrForbidden)
		return AuthorizationContext{}, TokenAccount{}, false
	}
	return resolved, account, true
}

func (s *Service) rechargeProducts(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.resolveContext(w, r); !ok {
		return
	}
	payload, err := s.billing.Products(r.Context())
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "the recharge catalog is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusOK, payload)
}

func (s *Service) createRechargeOrder(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolveContext(w, r)
	if !ok {
		return
	}
	if resolved.ContextType == "ORGANIZATION" && !slices.Contains(resolved.Roles, "OWNER") && !slices.Contains(resolved.Roles, "BILLING_ADMIN") {
		s.writeError(w, r, ErrForbidden)
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
	payload, err := s.billing.CreateRechargeOrder(r.Context(), resolved.UserID, walletID, body.ProductID, strings.ToUpper(body.Channel), r.Header.Get("Idempotency-Key"))
	if err != nil {
		api.WriteError(w, http.StatusBadGateway, "RECHARGE_CREATE_FAILED", "the recharge order could not be created", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusCreated, payload)
}

func (s *Service) getRechargeOrder(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolveContext(w, r)
	if !ok {
		return
	}
	orderID, err := uuid.Parse(r.PathValue("order_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid recharge order", httpserver.RequestID(r.Context()))
		return
	}
	payload, err := s.billing.RechargeOrder(r.Context(), orderID, resolved.UserID)
	if err != nil {
		api.WriteError(w, http.StatusNotFound, "RECHARGE_ORDER_NOT_FOUND", "the recharge order was not found", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusOK, payload)
}

func (s *Service) getRecentRechargeOrder(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolveContext(w, r)
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
	resolved, ok := s.resolveContext(w, r)
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

func (s *Service) getBalance(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolveContext(w, r)
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

func (s *Service) createTokenAccount(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolveContext(w, r)
	if !ok {
		return
	}
	var body struct {
		OwnerType         string     `json:"owner_type"`
		OwnerID           uuid.UUID  `json:"owner_id"`
		OwnerOrganization *uuid.UUID `json:"owner_organization_id"`
		OwnerUnit         *uuid.UUID `json:"owner_unit_id"`
		Name              string     `json:"name"`
		ShareScope        string     `json:"share_scope"`
		DefaultPriority   int        `json:"default_priority"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	body.OwnerType = strings.ToUpper(strings.TrimSpace(body.OwnerType))
	if body.DefaultPriority == 0 {
		body.DefaultPriority = 100
	}
	if !authorizeOwner(resolved, body.OwnerType, body.OwnerID, body.OwnerOrganization, body.OwnerUnit) {
		s.writeError(w, r, ErrForbidden)
		return
	}
	walletOwnerType := map[string]string{"USER": "PERSONAL", "ORG": "ORGANIZATION", "ORG_UNIT": "ORG_UNIT"}[body.OwnerType]
	walletID, err := s.wallets.Ensure(r.Context(), walletOwnerType, body.OwnerID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	account, err := s.repository.CreateAccount(r.Context(), CreateAccountRequest{OwnerType: body.OwnerType, OwnerID: body.OwnerID, OwnerOrganization: body.OwnerOrganization, OwnerUnit: body.OwnerUnit, WalletID: walletID, Name: body.Name, ShareScope: body.ShareScope, DefaultPriority: body.DefaultPriority, CreatedBy: resolved.UserID})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusCreated, account)
}

func (s *Service) rotateCredential(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolveContext(w, r)
	if !ok {
		return
	}
	accountID, err := uuid.Parse(r.PathValue("token_account_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid token account", httpserver.RequestID(r.Context()))
		return
	}
	account, err := s.repository.GetAccount(r.Context(), accountID)
	if err != nil || !authorizeOwner(resolved, account.OwnerType, account.OwnerID, account.OwnerOrganization, account.OwnerUnit) {
		s.writeError(w, r, ErrForbidden)
		return
	}
	var body struct {
		Provider      string   `json:"provider"`
		BaseURL       string   `json:"base_url"`
		Secret        string   `json:"secret"`
		AllowedModels []string `json:"allowed_models"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || strings.TrimSpace(body.Secret) == "" {
		return
	}
	secret := []byte(body.Secret)
	defer clear(secret)
	if err := s.validator.Validate(r.Context(), body.BaseURL, secret, body.AllowedModels); err != nil {
		api.WriteError(w, http.StatusUnprocessableEntity, "CREDENTIAL_VALIDATION_FAILED", "the new credential could not be validated", httpserver.RequestID(r.Context()))
		return
	}
	credential, err := s.repository.RotateCredential(r.Context(), accountID, resolved.UserID, body.Provider, body.BaseURL, secret, body.AllowedModels)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusCreated, credential)
}

func (s *Service) listEligible(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolveContext(w, r)
	if !ok {
		return
	}
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	if model == "" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "model is required", httpserver.RequestID(r.Context()))
		return
	}
	candidates, err := s.repository.ListCandidates(r.Context(), model)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	items := make([]map[string]any, 0)
	for _, candidate := range candidates {
		if Eligible(resolved, candidate, model) {
			items = append(items, map[string]any{"token_account_id": candidate.TokenAccountID, "name": candidate.Name, "owner_type": candidate.OwnerType, "allowed_models": candidate.AllowedModels, "health_status": candidate.HealthStatus})
		}
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "membership_version": resolved.MembershipVersion})
}

func (s *Service) getUsageRequest(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolveContext(w, r)
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
		"usage_request_id": usage.ID, "status": usage.Status, "display_status": usageDisplayStatus(usage),
		"token_account_id": usage.TokenAccountID, "wallet_id": usage.WalletID, "model": usage.Model,
		"maximum_microcredits": usage.MaximumMicrocredits, "actual_microcredits": usage.ActualMicrocredits,
		"started_at": usage.StartedAt, "updated_at": usage.UpdatedAt,
	})
}

func (s *Service) listMyUsageRequests(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolveContext(w, r)
	if !ok {
		return
	}
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "limit must be between 1 and 100", httpserver.RequestID(r.Context()))
			return
		}
		limit = parsed
	}
	usageRequests, err := s.repository.ListUsageForContext(r.Context(), resolved, limit)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(usageRequests))
	for _, usage := range usageRequests {
		items = append(items, map[string]any{
			"model":               usage.Model,
			"display_status":      usageDisplayStatus(usage),
			"actual_microcredits": usage.ActualMicrocredits,
			"started_at":          usage.StartedAt,
			"updated_at":          usage.UpdatedAt,
		})
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
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
	return resolved, true
}

func (s *Service) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := http.StatusInternalServerError, "INTERNAL_ERROR", "the token request could not be completed"
	switch {
	case errors.Is(err, ErrNotFound):
		status, code, message = http.StatusNotFound, "TOKEN_ACCOUNT_NOT_FOUND", "the token account was not found"
	case errors.Is(err, ErrForbidden):
		status, code, message = http.StatusForbidden, "TOKEN_FORBIDDEN", "the token action is not permitted"
	case errors.Is(err, ErrAmbiguousRoute):
		status, code, message = http.StatusConflict, "TOKEN_ROUTE_AMBIGUOUS", "multiple token routes have the same priority"
	case errors.Is(err, ErrNoEligibleRoute):
		status, code, message = http.StatusUnprocessableEntity, "TOKEN_ROUTE_UNAVAILABLE", "no eligible token route is available"
	}
	api.WriteError(w, status, code, message, httpserver.RequestID(r.Context()))
}

func authorizeOwner(context AuthorizationContext, ownerType string, ownerID uuid.UUID, organizationID, unitID *uuid.UUID) bool {
	switch ownerType {
	case "USER":
		return context.ContextType == "PERSONAL" && ownerID == context.UserID && organizationID == nil && unitID == nil
	case "ORG":
		return context.ContextType == "ORGANIZATION" && context.OrganizationID != nil && organizationID != nil && *context.OrganizationID == *organizationID && ownerID == *organizationID && (slices.Contains(context.Roles, "OWNER") || slices.Contains(context.Roles, "ORG_ADMIN"))
	case "ORG_UNIT":
		return context.ContextType == "ORGANIZATION" && context.OrganizationID != nil && organizationID != nil && unitID != nil && *context.OrganizationID == *organizationID && ownerID == *unitID && (slices.Contains(context.Roles, "OWNER") || slices.Contains(context.Roles, "ORG_ADMIN"))
	default:
		return false
	}
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
		return AuthorizationContext{}, ErrForbidden
	}
	var body struct {
		UserID            uuid.UUID   `json:"user_id"`
		SessionID         string      `json:"session_id"`
		ContextType       string      `json:"context_type"`
		OrganizationID    *uuid.UUID  `json:"organization_id"`
		MembershipVersion int64       `json:"membership_version"`
		UnitIDs           []uuid.UUID `json:"unit_ids"`
		AncestorUnitIDs   []uuid.UUID `json:"ancestor_unit_ids"`
		Roles             []string    `json:"roles"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&body); err != nil {
		return AuthorizationContext{}, err
	}
	return AuthorizationContext{UserID: body.UserID, SessionID: body.SessionID, ContextType: body.ContextType, OrganizationID: body.OrganizationID, MembershipVersion: body.MembershipVersion, UnitIDs: body.UnitIDs, AncestorUnitIDs: body.AncestorUnitIDs, Roles: body.Roles}, nil
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
	request.Header.Set("Authorization", "Bearer "+r.serviceToken)
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
