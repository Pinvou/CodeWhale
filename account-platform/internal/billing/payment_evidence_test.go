package billing

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestProviderMatchesFrozenOrderIdentityAndSKU(t *testing.T) {
	requesterID := uuid.MustParse("019c0000-0000-7000-8000-000000000001")
	order := PaymentOrder{MerchantOrderNo: "pv-1", RequesterID: requesterID, ProviderSKU: "8814A1D2", Channel: "ALIPAY", AmountFen: 20000, Currency: "CNY"}
	provider := ProviderOrder{MerchantOrderNo: "pv-1", ExternalCustomerKey: requesterID.String(), ExternalSKU: "8814A1D2", Channel: "ALIPAY", AmountFen: 20000, Currency: "CNY"}
	if !providerMatchesOrder(provider, order) {
		t.Fatal("matching Mall customer and SKU were rejected")
	}
	provider.ExternalCustomerKey = "019c0000-0000-7000-8000-000000000002"
	if providerMatchesOrder(provider, order) {
		t.Fatal("Mall customer mismatch was accepted")
	}
	provider.ExternalCustomerKey = requesterID.String()
	provider.ExternalSKU = "OTHER-SKU"
	if providerMatchesOrder(provider, order) {
		t.Fatal("Mall SKU mismatch was accepted")
	}
}

func TestProviderFailureMatchesOrderRequiresExactUnpaidIdentity(t *testing.T) {
	order := PaymentOrder{MerchantOrderNo: "order-1", RequesterID: uuid.MustParse("11111111-1111-1111-1111-111111111111"), ProviderSKU: "sku-1", Channel: "ALIPAY", AmountFen: 20000, Currency: "CNY"}
	provider := ProviderOrder{MerchantOrderNo: order.MerchantOrderNo, ExternalCustomerKey: order.RequesterID.String(), ExternalSKU: order.ProviderSKU, Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, Status: "REVIEW_REQUIRED", ProviderStatus: "FAILED"}
	if !providerFailureMatchesOrder(provider, order) {
		t.Fatal("exact FAILED provider fact should be confirmable")
	}
	provider.ExternalCustomerKey = ""
	if providerFailureMatchesOrder(provider, order) {
		t.Fatal("missing permanent customer identity must fail closed")
	}
	provider.ExternalCustomerKey = order.RequesterID.String()
	transactionID := "provider-payment-1"
	provider.ChannelTransactionID = &transactionID
	if providerFailureMatchesOrder(provider, order) {
		t.Fatal("FAILED fact with a payment transaction must fail closed")
	}
}

func TestClassifyPaymentTransaction(t *testing.T) {
	expiresAt := time.Date(2026, time.August, 23, 10, 0, 0, 0, time.UTC)
	allowLate := PaymentPolicy{MaximumLateConfirmation: time.Hour}

	tests := []struct {
		name            string
		paidAt          time.Time
		policy          PaymentPolicy
		prior           int
		repair          bool
		wantStatus      string
		wantFulfillment string
		wantReview      bool
	}{
		{name: "on-time payment", paidAt: expiresAt.Add(-time.Minute), policy: allowLate, wantStatus: "PAID", wantFulfillment: "PENDING"},
		{name: "allowed late payment", paidAt: expiresAt.Add(time.Minute), policy: allowLate, wantStatus: "LATE_PAID", wantFulfillment: "PENDING"},
		{name: "late payment beyond policy", paidAt: expiresAt.Add(2 * time.Hour), policy: allowLate, wantStatus: "REVIEW_REQUIRED", wantFulfillment: "REVIEW_REQUIRED", wantReview: true},
		{name: "second distinct transaction", paidAt: expiresAt.Add(-time.Minute), policy: allowLate, prior: 1, wantStatus: "REVIEW_REQUIRED", wantFulfillment: "REVIEW_REQUIRED", wantReview: true},
		{name: "verified repair", paidAt: expiresAt.Add(time.Minute), policy: PaymentPolicy{ReviewLatePayment: true}, prior: 1, repair: true, wantStatus: "LATE_PAID", wantFulfillment: "PENDING"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status, fulfillment, review := classifyPaymentTransaction(expiresAt, test.paidAt, test.policy, test.prior, test.repair)
			if status != test.wantStatus || fulfillment != test.wantFulfillment || review != test.wantReview {
				t.Fatalf("classifyPaymentTransaction() = (%q, %q, %t), want (%q, %q, %t)", status, fulfillment, review, test.wantStatus, test.wantFulfillment, test.wantReview)
			}
		})
	}
}

func TestClassifyPaymentTransactionAfterLocalClose(t *testing.T) {
	expiresAt := time.Date(2026, time.August, 23, 10, 0, 0, 0, time.UTC)
	status, fulfillment, review := classifyPaymentTransactionAfterClose(
		expiresAt,
		expiresAt.Add(-time.Minute),
		"LOCALLY_CLOSED_EXTERNAL_PENDING",
		PaymentPolicy{MaximumLateConfirmation: time.Hour},
		0,
		false,
	)
	if status != "LATE_PAID" || fulfillment != "PENDING" || review {
		t.Fatalf("closed payment classification = (%q, %q, %t), want LATE_PAID, PENDING, false", status, fulfillment, review)
	}
}
