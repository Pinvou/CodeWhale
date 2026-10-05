package billing

import "testing"

func TestFilterProductsRejectsTentativePricesByDefault(t *testing.T) {
	coordinator := &PaymentCoordinator{productSKUs: map[string]ExternalProduct{
		"PLAN_TRIAL": {SKU: "0502AJON", AmountFen: 10},
	}}
	products := []Product{{ProductID: "PLAN_TRIAL", AmountFen: 10, PriceStatus: "UAT_TENTATIVE"}}
	if got := coordinator.FilterProducts(products); len(got) != 0 {
		t.Fatalf("tentative catalog leaked without explicit opt-in: %#v", got)
	}
	if got := coordinator.AllowTentativePrices(true).FilterProducts(products); len(got) != 1 {
		t.Fatalf("UAT opt-in did not expose tentative catalog: %#v", got)
	}
}

func TestFilterProductsAllowsFrozenPrices(t *testing.T) {
	coordinator := &PaymentCoordinator{productSKUs: map[string]ExternalProduct{
		"PLAN_STANDARD": {SKU: "0502AJ0Q", AmountFen: 1000},
	}}
	products := []Product{{ProductID: "PLAN_STANDARD", AmountFen: 1000, PriceStatus: "FROZEN"}}
	if got := coordinator.FilterProducts(products); len(got) != 1 {
		t.Fatalf("frozen catalog was rejected: %#v", got)
	}
}
