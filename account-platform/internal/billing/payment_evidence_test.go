package billing

import (
	"testing"
	"time"
)

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
