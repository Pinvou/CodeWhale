package billing

import (
	"context"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/paymentcredit"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

// Historical allocation fixtures still need the current payment-credit receipt
// before their real RECHARGE journal. Read the actual order in the same tx;
// only Account's response is controlled, not the Billing receipt or DB guard.
func prepareBillingFixturePaymentCredit(t *testing.T, ctx context.Context, coordinator *PaymentCoordinator, tx pgx.Tx, orderID, transactionID uuid.UUID, providerReference string, paidAt time.Time) uuid.UUID {
	t.Helper()
	order, err := getPaymentOrderByID(ctx, tx, orderID, false)
	if err != nil {
		t.Fatal(err)
	}
	provider := ProviderOrder{MerchantOrderNo: order.MerchantOrderNo, Status: "SUCCESS", Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, ChannelTransactionID: &providerReference, PaidAt: &paidAt, ExternalCustomerKey: order.RequesterID.String()}
	id, admitted, err := coordinator.preparePaymentCredit(ctx, tx, order, transactionID, provider)
	if err != nil || !admitted {
		t.Fatal("controlled allocation funding needs current Account admission", err)
	}
	return id
}
