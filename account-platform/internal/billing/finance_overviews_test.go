package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
)

func TestFinancePaymentsWindowDefaultsToLast24Hours(t *testing.T) {
	now := time.Date(2026, time.September, 2, 12, 0, 0, 123, time.UTC)
	window, err := parseFinancePaymentsWindow(url.Values{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !window.From.Equal(now.Add(-24*time.Hour)) || !window.To.Equal(now) || window.Timezone != "UTC" {
		t.Fatalf("unexpected default window: %#v", window)
	}
}

func TestFinancePaymentsWindowRequiresBoundedUTCHalfOpenPair(t *testing.T) {
	valid := url.Values{"from": {"2026-08-01T00:00:00Z"}, "to": {"2026-09-01T00:00:00+00:00"}}
	window, err := parseFinancePaymentsWindow(valid, time.Now())
	if err != nil || window.To.Sub(window.From) != 31*24*time.Hour {
		t.Fatalf("valid 31-day UTC window rejected: %#v %v", window, err)
	}
	invalid := []url.Values{
		{"from": {"2026-08-01T00:00:00Z"}},
		{"from": {"2026-08-01T00:00:00Z"}, "to": {"2026-08-01T00:00:00Z"}},
		{"from": {"2026-08-01T00:00:00+08:00"}, "to": {"2026-08-02T00:00:00+08:00"}},
		{"from": {"2026-08-01T00:00:00Z"}, "to": {"2026-09-01T00:00:01Z"}},
		{"from": {"2026-08-01T00:00:00Z"}, "to": {"2026-08-02T00:00:00Z"}, "timezone": {"UTC"}},
	}
	for _, query := range invalid {
		if _, err := parseFinancePaymentsWindow(query, time.Now()); err == nil {
			t.Fatalf("invalid window accepted: %#v", query)
		}
	}
}

func TestFinanceOverviewDoesNotReturnPartialMetricsWithoutRepository(t *testing.T) {
	service := NewService(nil)
	request := httptest.NewRequest(http.MethodGet, "/admin/v1/finance-overviews/accounts", nil)
	response := httptest.NewRecorder()
	service.adminFinanceAccountsOverview(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["code"] != "FINANCE_OVERVIEW_UNAVAILABLE" || payload["metrics"] != nil {
		t.Fatalf("unexpected fail-closed response: %#v", payload)
	}
}

type failingFinanceOverviewRepository struct{ Repository }

func (failingFinanceOverviewRepository) AdminFinanceAccountsOverview(context.Context, time.Time) (financeAccountsOverviewValues, error) {
	return financeAccountsOverviewValues{}, errors.New("query failed")
}

func (failingFinanceOverviewRepository) AdminFinancePaymentsOverview(context.Context, time.Time, time.Time, time.Time) (financePaymentsOverviewValues, error) {
	return financePaymentsOverviewValues{}, errors.New("query failed")
}

func (failingFinanceOverviewRepository) AdminFinancePricingOverview(context.Context, time.Time) (financePricingOverviewValues, error) {
	return financePricingOverviewValues{}, errors.New("query failed")
}

func (failingFinanceOverviewRepository) AdminFinanceProductsOverview(context.Context, time.Time) (financeProductsOverviewValues, error) {
	return financeProductsOverviewValues{}, errors.New("query failed")
}

func TestEveryFinanceOverviewFailsAtomicallyWhenItsQueryFails(t *testing.T) {
	service := NewService(failingFinanceOverviewRepository{})
	for _, testCase := range []struct {
		path    string
		handler http.HandlerFunc
	}{
		{path: "/admin/v1/finance-overviews/accounts", handler: service.adminFinanceAccountsOverview},
		{path: "/admin/v1/finance-overviews/payments", handler: service.adminFinancePaymentsOverview},
		{path: "/admin/v1/finance-overviews/pricing", handler: service.adminFinancePricingOverview},
		{path: "/admin/v1/finance-overviews/products", handler: service.adminFinanceProductsOverview},
	} {
		request := httptest.NewRequest(http.MethodGet, testCase.path, nil)
		response := httptest.NewRecorder()
		testCase.handler(response, request)
		var payload map[string]any
		if response.Code != http.StatusServiceUnavailable || json.Unmarshal(response.Body.Bytes(), &payload) != nil || payload["code"] != "FINANCE_OVERVIEW_UNAVAILABLE" || payload["metrics"] != nil {
			t.Fatalf("%s leaked a partial result: status=%d body=%s", testCase.path, response.Code, response.Body.String())
		}
	}
}

func TestPointInTimeFinanceOverviewRejectsWindowParameters(t *testing.T) {
	service := NewService(nil)
	request := httptest.NewRequest(http.MethodGet, "/admin/v1/finance-overviews/pricing?from=2026-08-01T00:00:00Z", nil)
	response := httptest.NewRecorder()
	service.adminFinancePricingOverview(response, request)
	if response.Code != http.StatusBadRequest || !json.Valid(response.Body.Bytes()) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestFinanceOverviewRoutesAuthorizeEachDomainIndependently(t *testing.T) {
	var allowedPermission string
	var observedPermission string
	authorizationServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Permission string `json:"permission"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		observedPermission = body.Permission
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": body.Permission == allowedPermission, "user_id": "00000000-0000-7000-8000-000000000001"})
	}))
	t.Cleanup(authorizationServer.Close)
	authorizer, err := authorization.NewClient(authorizationServer.URL, "test-service-token")
	if err != nil {
		t.Fatal(err)
	}
	verify := func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "test", Subject: "finance-reviewer"}, nil
	}
	handler := NewService(nil).WithAuthorizer(authorizer).Handler("", verify)
	for _, testCase := range []struct {
		resource   string
		permission string
	}{
		{resource: "accounts", permission: "platform.billing.wallets.read"},
		{resource: "payments", permission: "platform.billing.wallets.read"},
		{resource: "pricing", permission: "platform.pricing.read"},
		{resource: "products", permission: "platform.products.read"},
	} {
		allowedPermission = testCase.permission
		observedPermission = ""
		request := httptest.NewRequest(http.MethodGet, "/admin/v1/finance-overviews/"+testCase.resource, nil)
		request.Header.Set("Authorization", "Bearer valid")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if observedPermission != testCase.permission || response.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s used permission %q and returned %d", testCase.resource, observedPermission, response.Code)
		}
	}
}
