package billing

import (
	"context"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/paymentcredit"
)

// Existing Billing-only financial tests control Account admission explicitly.
// They still execute the real Billing admission receipt, journal trigger and
// money transaction. Account cleanup/unknown-outcome correctness is exercised
// separately with real Account/Billing/Token roles and public migrations.
type billingFixturePaymentCreditAdmission struct{}

func (billingFixturePaymentCreditAdmission) Admit(_ context.Context, input paymentcredit.Request) (paymentcredit.Decision, error) {
	if !input.Valid() {
		return paymentcredit.Decision{}, paymentcredit.ErrProtocol
	}
	return paymentcredit.Decision{ID: input.ID, Fingerprint: input.Fingerprint(), State: "ADMITTED"}, nil
}

func newBillingFixturePaymentCoordinator(repository *PostgresRepository, provider PaymentProvider, environment ...string) (*PaymentCoordinator, error) {
	coordinator, err := NewPaymentCoordinatorWithProvider(repository, provider, environment...)
	if err != nil {
		return nil, err
	}
	return coordinator.WithPaymentCreditAdmission(billingFixturePaymentCreditAdmission{}), nil
}
