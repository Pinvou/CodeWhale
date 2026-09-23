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
	return payment.Capabilities{ProviderID: "test", Version: "v1", Channels: []string{"ALIPAY"}, QRScanApps: []string{"ALIPAY", "WECHAT"}}
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
	coordinator := &PaymentCoordinator{provider: catalogProvider{}}
	mappings := map[string]ExternalProduct{"PLAN_TRIAL": testExternalProduct("0502AJON", 4900)}
	products := []Product{{ProductID: "PLAN_TRIAL", AmountFen: 4900, Currency: "CNY", Channels: []string{"ALIPAY"}, PriceStatus: "UAT_TENTATIVE"}}
	if got := coordinator.filterProducts(products, mappings); len(got) != 0 {
		t.Fatalf("tentative catalog leaked without explicit opt-in: %#v", got)
	}
	if got := coordinator.AllowTentativePrices(true).filterProducts(products, mappings); len(got) != 1 {
		t.Fatalf("UAT opt-in did not expose tentative catalog: %#v", got)
	}
}

func TestFilterProductsAllowsFrozenPrices(t *testing.T) {
	coordinator := &PaymentCoordinator{provider: catalogProvider{}}
	mappings := map[string]ExternalProduct{"PLAN_STANDARD": testExternalProduct("0502AJ0Q", 9900)}
	products := []Product{{ProductID: "PLAN_STANDARD", AmountFen: 9900, Currency: "CNY", Channels: []string{"ALIPAY"}, PriceStatus: "FROZEN"}}
	if got := coordinator.filterProducts(products, mappings); len(got) != 1 || len(got[0].QRScanApps) != 2 || got[0].QRScanApps[0] != "ALIPAY" || got[0].QRScanApps[1] != "WECHAT" {
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

func TestCatalogFingerprintBindsEveryPublishedFact(t *testing.T) {
	item := RechargeProductCatalogItem{
		CatalogItemID: uuid.Must(uuid.NewV7()), ProductID: "PRODUCT",
		DisplayNameZhCN: "zh", DisplayNameEN: "en", DisplayNameJA: "ja",
		Mapping: RechargeProductMappingDraft{DraftVersion: 1, ProviderID: "provider", ProviderVersion: "v1", ExternalSKUBOM: "sku", AmountFen: 100, Currency: "CNY", Channels: []string{"ALIPAY"}, AdmissionClass: "FORMAL"},
	}
	product := Product{ProductID: item.ProductID, BasePoints: 10, BonusPoints: 2, ValidityDays: 30, InvoiceProductName: "invoice"}
	original := catalogFingerprint(item, product)
	mutations := []func(){
		func() { item.DisplayNameEN = "changed" },
		func() { product.BasePoints++ },
		func() { product.BonusPoints++ },
		func() { product.ValidityDays++ },
		func() { product.InvoiceProductName = "changed" },
		func() { item.Mapping.ProviderVersion = "v2" },
		func() { item.Mapping.ExternalSKUBOM = "changed" },
		func() { item.Mapping.AmountFen++ },
		func() { item.Mapping.Channels = []string{"WECHAT"} },
	}
	for index, mutate := range mutations {
		beforeItem, beforeProduct := item, product
		mutate()
		if catalogFingerprint(item, product) == original {
			t.Fatalf("mutation %d did not change publish fingerprint", index)
		}
		item, product = beforeItem, beforeProduct
	}
}
