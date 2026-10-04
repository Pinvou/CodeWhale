// This test fixture serializes the production response types and probes the
// production HTTP router's validation paths. It does not perform a payment.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/token"
	"github.com/google/uuid"
)

type contractRepository struct {
	token.Repository
	candidate token.Candidate
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

func main() {
	id := uuid.New()
	paidAt := time.Now().UTC()
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
	if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"product": product, "paid": order, "created": created, "wallet": wallet, "routes": routes, "eligible": eligible, "token_errors": errors, "recent_created": map[string]any{"order": created}, "recent_empty": map[string]any{"order": nil}}); err != nil {
		panic(err)
	}
}
