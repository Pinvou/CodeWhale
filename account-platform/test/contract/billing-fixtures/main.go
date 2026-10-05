// This test fixture serializes the production response types and probes the
// production HTTP router's validation paths. It does not perform a payment.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/token"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type contractRepository struct {
	token.Repository
	candidate token.Candidate
}

func (contractRepository) AdminOverview(context.Context) (token.AdminOverview, error) {
	return token.AdminOverview{}, nil
}
func (contractRepository) AdminTokenAccounts(context.Context, int) ([]token.AdminTokenAccount, error) {
	return []token.AdminTokenAccount{}, nil
}
func (contractRepository) AdminUsageRequests(context.Context, int) ([]token.AdminUsageRequest, error) {
	amount := int64(9007199254740993)
	requestID, lastError := "fixture-newapi-request", "fixture-reconciliation-error"
	return []token.AdminUsageRequest{{UserID: uuid.New(), TokenAccountID: uuid.New(), CredentialID: uuid.New(), WalletID: uuid.New(), NewAPIRequestID: &requestID, ReconciliationLastError: &lastError, ID: uuid.New(), Model: "fixture-model", Status: "COMMITTED", MaximumMicrocredits: amount, ActualMicrocredits: &amount, StartedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}}, nil
}
func (contractRepository) ListUsageForContext(context.Context, token.AuthorizationContext, int, *token.UsageHistoryCursor) ([]token.UserUsageRecord, error) {
	amount := int64(9007199254740993)
	return []token.UserUsageRecord{{UsageRequestID: uuid.New(), Model: "fixture-model", DisplayStatus: "SETTLED", ActualMicrocredits: &amount, StartedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}}, nil
}

func (contractRepository) AdminTokenAccount(context.Context, uuid.UUID) (token.AdminTokenAccountDetail, error) {
	return token.AdminTokenAccountDetail{}, errors.New("fixture operation not implemented")
}
func (contractRepository) AdminDeleteRoute(context.Context, uuid.UUID, string, uuid.UUID) error {
	return errors.New("fixture operation not implemented")
}
func (contractRepository) AdminCredentialMetadata(context.Context, uuid.UUID, uuid.UUID) (token.AdminCredentialMetadata, error) {
	return token.AdminCredentialMetadata{}, errors.New("fixture operation not implemented")
}
func (contractRepository) AdminRecordCredentialValidation(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, bool) (token.AdminCredentialMetadata, error) {
	return token.AdminCredentialMetadata{}, errors.New("fixture operation not implemented")
}
func (contractRepository) AdminUsageRequest(context.Context, uuid.UUID) (token.AdminUsageRequest, error) {
	return token.AdminUsageRequest{}, errors.New("fixture operation not implemented")
}
func (contractRepository) AdminRetryReconciliation(context.Context, uuid.UUID, string, uuid.UUID) (token.AdminUsageRequest, error) {
	return token.AdminUsageRequest{}, errors.New("fixture operation not implemented")
}

type adminBillingRepository struct{ billing.Repository }

func (adminBillingRepository) AdminOverview(context.Context) (billing.AdminOverview, error) {
	return billing.AdminOverview{PostedMicrocredits: "9007199254740993", ReservedMicrocredits: "1"}, nil
}
func (adminBillingRepository) AdminWallets(context.Context, int) ([]billing.Wallet, error) {
	return []billing.Wallet{{PostedMicrocredits: 9007199254740993, ReservedMicrocredits: 1, AvailableMicrocredits: 9007199254740992}}, nil
}
func (adminBillingRepository) AdminPaymentOrders(context.Context, int) ([]billing.AdminPaymentOrder, error) {
	return []billing.AdminPaymentOrder{{AmountFen: 9007199254740993, CreditMicrocredits: 9007199254740993}}, nil
}
func (adminBillingRepository) AdminAdjustments(context.Context, int) ([]billing.ManualAdjustment, error) {
	return []billing.ManualAdjustment{{AmountMicrocredits: -9007199254740993}}, nil
}
func (adminBillingRepository) AdminUsageHolds(context.Context, int) ([]billing.AdminHold, error) {
	amount := int64(9007199254740993)
	return []billing.AdminHold{{MaxMicrocredits: amount, ActualMicrocredits: &amount}}, nil
}

func (adminBillingRepository) AdminPriceVersions(context.Context, int) ([]billing.AdminPriceVersion, error) {
	return []billing.AdminPriceVersion{{ID: uuid.New(), Model: "fixture-model", InputPerMillion: 9007199254740993, OutputPerMillion: 1, CachedInputPerMillion: 2, EffectiveFrom: time.Now().UTC(), CreatedAt: time.Now().UTC()}}, nil
}
func (adminBillingRepository) AdminCreatePriceVersion(context.Context, string, int64, int64, int64) (billing.AdminPriceVersion, error) {
	return billing.AdminPriceVersion{}, errors.New("fixture operation not implemented")
}
func (adminBillingRepository) AdminWalletLedger(context.Context, uuid.UUID, int) ([]billing.AdminLedgerEntry, error) {
	return []billing.AdminLedgerEntry{{PostingID: uuid.New(), JournalEntryID: uuid.New(), DeltaMicrocredit: -9007199254740993, CreatedAt: time.Now().UTC()}}, nil
}
func (adminBillingRepository) AdminPaymentOrder(context.Context, uuid.UUID) (billing.AdminPaymentOrder, error) {
	return billing.AdminPaymentOrder{}, errors.New("fixture operation not implemented")
}
func (adminBillingRepository) AdminReconciliationRuns(context.Context, int) ([]billing.ReconciliationRun, error) {
	return nil, errors.New("fixture operation not implemented")
}
func (adminBillingRepository) AdminReconciliationCases(context.Context, uuid.UUID, int) ([]billing.AdminReconciliationCase, error) {
	return nil, errors.New("fixture operation not implemented")
}

func financialReadProbes() map[string]json.RawMessage {
	verify := func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{RealmRoles: []string{"platform-admin"}}, nil
	}
	billingHandler := billing.NewService(adminBillingRepository{}).Handler("fixture-service-token", verify)
	tokenHandler := token.NewService(contractRepository{}, contractContext{value: token.AuthorizationContext{UserID: uuid.New(), ContextType: "PERSONAL"}}, nil, nil, token.ProviderURLPolicy{}, nil, nil, 100).Handler(verify)
	results := map[string]json.RawMessage{}
	for name, handler := range map[string]http.Handler{"billing": billingHandler, "token": tokenHandler} {
		paths := []string{"/admin/v1/overview", "/admin/v1/wallets", "/admin/v1/payment-orders", "/admin/v1/adjustments", "/admin/v1/usage-holds", "/admin/v1/pricing-versions", "/admin/v1/wallets/" + uuid.NewString() + "/ledger"}
		if name == "token" {
			paths = []string{"/admin/v1/usage-requests", "/v1/me/usage-requests"}
		}
		for _, path := range paths {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.Header.Set("Authorization", "Bearer fixture-only-token")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				panic("financial read probe did not reach production response")
			}
			key := name + path
			if strings.HasSuffix(path, "/ledger") {
				key = "billing/admin/v1/wallets/fixture/ledger"
			}
			results[key] = response.Body.Bytes()
		}
	}
	return results
}

// Controlled dependencies, with the actual public administration handler.
// Real cross-domain PostgreSQL/HTTP financial effects are tested separately.
type waiverReadRepository struct {
	adminBillingRepository
	hold billing.AdminHold
}

func (r waiverReadRepository) AdminUsageHold(context.Context, uuid.UUID) (billing.AdminHold, error) {
	return r.hold, nil
}

type waiverReadCoordinator struct{ hold billing.AdminHold }

func (c waiverReadCoordinator) Release(_ context.Context, holdID, usageID uuid.UUID, reason, actor string) (billing.AdminHold, error) {
	if c.hold.ID != holdID || c.hold.UsageRequestID != usageID || reason == "" || actor == "" {
		return billing.AdminHold{}, billing.ErrConflict
	}
	return c.hold, nil
}
func publicWaiverProbe(hold billing.AdminHold) json.RawMessage {
	handler := billing.NewService(waiverReadRepository{hold: hold}).WithWaiverCoordinator(waiverReadCoordinator{hold: hold}).Handler("fixture-service-token", func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "fixture-issuer", Subject: "fixture-actor", RealmRoles: []string{"platform-admin"}}, nil
	})
	request := httptest.NewRequest("POST", "/admin/v1/usage-holds/"+hold.ID.String()+"/release", strings.NewReader(`{"reason":"fixture-waiver"}`))
	request.Header.Set("Authorization", "Bearer fixture-admin")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 {
		panic("public waiver probe did not reach the production success contract")
	}
	return response.Body.Bytes()
}

func (r contractRepository) ListCandidates(context.Context, string) ([]token.Candidate, error) {
	return []token.Candidate{r.candidate}, nil
}

type contractContext struct {
	value token.AuthorizationContext
	err   error
}

func (c contractContext) Resolve(context.Context, string) (token.AuthorizationContext, error) {
	return c.value, c.err
}

func eligibleProbe(repository token.Repository, contexts token.ContextResolver) json.RawMessage {
	service := token.NewService(repository, contexts, nil, nil, token.ProviderURLPolicy{}, nil, nil, 100)
	application := service.Handler(func(context.Context, string) (identity.Principal, error) { return identity.Principal{}, nil })
	handler := httpserver.HandlerWithApplication(config.Config{ServiceName: "contract-token"}, application, nil)
	request := httptest.NewRequest(http.MethodGet, "/v1/token-accounts/eligible?model=fixture-model", nil)
	request.Header.Set("Authorization", "Bearer fixture-only-access-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response.Body.Bytes()
}

func rateLimitProbe() json.RawMessage {
	handler := httpserver.HandlerWithApplication(config.Config{ServiceName: "contract-rate", RateLimitRPS: 0.001, RateLimitBurst: 1}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }), nil)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/business", nil))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/business", nil))
	if response.Code != 429 || response.Header().Get("Retry-After") != "1" {
		panic("rate limit middleware did not return its production 429 contract")
	}
	return response.Body.Bytes()
}

func main() {
	// Keep production HTTP JSON logs separate from the single fixture document.
	output := os.Stdout
	os.Stdout = os.Stderr
	gin.DefaultWriter = os.Stderr
	gin.DefaultErrorWriter = os.Stderr
	id := uuid.New()
	paidAt := time.Now().UTC()
	zero := int64(0)
	waiver := billing.WaiverAuthority{Hold: billing.AdminHold{ID: id, UsageRequestID: id, WalletID: id, Status: "RELEASED", MaxMicrocredits: 9007199254740993, ActualMicrocredits: &zero, ExpiresAt: paidAt.Add(time.Hour), UpdatedAt: paidAt}, UsageSnapshot: json.RawMessage(`{"admin_waiver":true,"admin_actor":"fixture-actor","admin_release_reason":"fixture-waiver","usage":{"input_tokens":7,"output_tokens":0,"cached_input_tokens":0}}`)}
	transaction := "test-transaction"
	order := billing.PaymentOrder{ID: id, MerchantOrderNo: "test-order", RequesterID: id, WalletID: id, ProductID: "CNY_1", Channel: "ALIPAY", AmountFen: 100, Currency: "CNY", CreditMicrocredits: 1000000, Status: "PAID", FulfillmentStatus: "CREDITED", PaymentURL: "http://127.0.0.1/test-payment", ChannelTransactionID: &transaction, ExpiresAt: paidAt.Add(time.Minute), PaidAt: &paidAt}
	created := order
	created.Status, created.FulfillmentStatus = "CREATED", "PENDING"
	created.PaymentURL, created.ChannelTransactionID, created.PaidAt = "", nil, nil
	product := billing.Product{ProductID: "CNY_1", AmountFen: 100, CreditMicrocredits: 1000000, DisplayPoints: 100, Currency: "CNY", Channels: []string{"ALIPAY"}, CatalogVersion: 1}
	wallet := billing.Wallet{ID: id, OwnerType: "PERSONAL", OwnerID: id, Status: "ACTIVE", PostedMicrocredits: 1000000, ReservedMicrocredits: 1000, AvailableMicrocredits: 999000, Version: 1}
	handler := billing.NewService(nil).WithPayments(&billing.PaymentCoordinator{}).Handler("fixture-only-service-token")
	routes := map[string]int{}
	for _, probe := range []struct{ method, path string }{
		{http.MethodPost, "/internal/v1/recharge/orders"},
		{http.MethodGet, "/internal/v1/recharge/orders/recent"},
		{http.MethodGet, "/internal/v1/recharge/orders/" + id.String()},
	} {
		request := httptest.NewRequest(probe.method, probe.path, strings.NewReader(`{}`))
		request.Header.Set("Authorization", "Bearer fixture-only-service-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		routes[probe.method+" "+probe.path] = response.Code
	}
	eligible := eligibleProbe(contractRepository{candidate: token.Candidate{TokenAccountID: id, OwnerType: "USER", OwnerID: id, Name: "fixture", Status: "ACTIVE", HealthStatus: "HEALTHY", CredentialID: id, WalletID: id, AllowedModels: []string{"fixture-model"}}}, contractContext{value: token.AuthorizationContext{UserID: id, ContextType: "PERSONAL", MembershipVersion: 7}})
	errors := map[string]json.RawMessage{}
	for code, cause := range map[string]error{"TOKEN_ACCOUNT_NOT_FOUND": token.ErrNotFound, "TOKEN_FORBIDDEN": token.ErrForbidden, "TOKEN_ROUTE_AMBIGUOUS": token.ErrAmbiguousRoute, "TOKEN_ROUTE_UNAVAILABLE": token.ErrNoEligibleRoute} {
		errors[code] = eligibleProbe(nil, contractContext{err: cause})
	}
	if err := json.NewEncoder(output).Encode(map[string]any{"waiver_public": publicWaiverProbe(waiver.Hold), "waiver_authority": waiver, "waiver_intent": map[string]any{"hold_id": id, "usage_request_id": id, "reason": "fixture-waiver", "actor": "fixture-actor"}, "product": product, "paid": order, "created": created, "wallet": wallet, "routes": routes, "eligible": eligible, "token_errors": errors, "rate_limited": rateLimitProbe(), "recent_created": map[string]any{"order": created}, "recent_empty": map[string]any{"order": nil}, "financial_reads": financialReadProbes(), "organization_member": account.Member{MembershipID: id, UserID: id, Status: "ACTIVE", Version: 1, Roles: []string{"MEMBER", "UNIT_ADMIN"}, UnitIDs: []uuid.UUID{}, UnitAdminIDs: []uuid.UUID{id}, JoinedAt: paidAt}}); err != nil {
		panic(err)
	}
}
