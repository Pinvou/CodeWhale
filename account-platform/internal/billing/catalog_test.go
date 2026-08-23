package billing

import (
	"context"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/google/uuid"
)

type catalogProvider struct{}

func (catalogProvider) Capabilities() payment.Capabilities {
	return payment.Capabilities{ProviderID: "test", Version: "v1", Channels: []string{"ALIPAY"}}
}
func (catalogProvider) Create(context.Context, payment.Order, string) (payment.ProviderOrder, error) {
	return payment.ProviderOrder{}, payment.ErrUnsupported
}
func (catalogProvider) Get(context.Context, string) (payment.ProviderOrder, error) {
	return payment.ProviderOrder{}, payment.ErrUnsupported
}
func (catalogProvider) Close(context.Context, string) (payment.ProviderOrder, error) {
	return payment.ProviderOrder{}, payment.ErrUnsupported
}
func (catalogProvider) Refund(context.Context, string, string, int64) (payment.Refund, error) {
	return payment.Refund{}, payment.ErrUnsupported
}
func (catalogProvider) Bill(context.Context, string, time.Time) ([]payment.BillEntry, error) {
	return nil, payment.ErrUnsupported
}

func testExternalProduct(sku string, amountFen int64) ExternalProduct {
	return ExternalProduct{MappingID: uuid.Must(uuid.NewV7()), MappingVersion: 1, ProviderID: "test", ProviderVersion: "v1", SKU: sku, AmountFen: amountFen, Currency: "CNY", Channels: []string{"ALIPAY"}, Status: "TEST_ONLY"}
}

func TestFilterProductsRejectsTentativePricesByDefault(t *testing.T) {
	coordinator := &PaymentCoordinator{provider: catalogProvider{}, productSKUs: map[string]ExternalProduct{
		"PLAN_TRIAL": testExternalProduct("0502AJON", 10),
	}}
	products := []Product{{ProductID: "PLAN_TRIAL", AmountFen: 10, Currency: "CNY", Channels: []string{"ALIPAY"}, PriceStatus: "UAT_TENTATIVE"}}
	if got := coordinator.FilterProducts(products); len(got) != 0 {
		t.Fatalf("tentative catalog leaked without explicit opt-in: %#v", got)
	}
	if got := coordinator.AllowTentativePrices(true).FilterProducts(products); len(got) != 1 {
		t.Fatalf("UAT opt-in did not expose tentative catalog: %#v", got)
	}
}

func TestFilterProductsAllowsFrozenPrices(t *testing.T) {
	coordinator := &PaymentCoordinator{provider: catalogProvider{}, productSKUs: map[string]ExternalProduct{
		"PLAN_STANDARD": testExternalProduct("0502AJ0Q", 1000),
	}}
	products := []Product{{ProductID: "PLAN_STANDARD", AmountFen: 1000, Currency: "CNY", Channels: []string{"ALIPAY"}, PriceStatus: "FROZEN"}}
	if got := coordinator.FilterProducts(products); len(got) != 1 {
		t.Fatalf("frozen catalog was rejected: %#v", got)
	}
}

func TestPaymentPolicyReviewsOnlyLateConfirmationsOutsideCapabilityWindow(t *testing.T) {
	expiresAt := time.Now().UTC()
	policy := PaymentPolicy{MaximumLateConfirmation: time.Minute}
	if policy.RequiresReview(expiresAt, expiresAt.Add(30*time.Second)) {
		t.Fatal("confirmation inside the declared late window required review")
	}
	if !policy.RequiresReview(expiresAt, expiresAt.Add(2*time.Minute)) {
		t.Fatal("confirmation outside the declared late window bypassed review")
	}
	policy.ReviewLatePayment = true
	if !policy.RequiresReview(expiresAt, expiresAt) {
		t.Fatal("provider policy requiring all late payments to be reviewed was ignored")
	}
}
