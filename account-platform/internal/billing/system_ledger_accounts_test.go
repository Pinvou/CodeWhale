package billing

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestValidateSystemLedgerAccountSet(t *testing.T) {
	actual := make(map[uuid.UUID]systemLedgerAccountActual, len(systemLedgerAccountExpectations))
	for _, expected := range systemLedgerAccountExpectations {
		actual[expected.ID] = systemLedgerAccountActual{Code: expected.Code, AccountType: expected.AccountType}
	}
	if err := validateSystemLedgerAccountSet(actual); err != nil {
		t.Fatalf("valid system ledger accounts were rejected: %v", err)
	}

	delete(actual, paymentClearingAccount)
	if err := validateSystemLedgerAccountSet(actual); err == nil {
		t.Fatal("missing payment clearing account was accepted")
	}

	actual[paymentClearingAccount] = systemLedgerAccountActual{Code: "platform:payment-clearing", AccountType: "WALLET_LIABILITY"}
	if err := validateSystemLedgerAccountSet(actual); err == nil {
		t.Fatal("mutated payment clearing account was accepted")
	}
}

func TestPaymentNotificationErrorCode(t *testing.T) {
	postgresError := &pgconn.PgError{Code: "23503", ConstraintName: "ledger_postings_ledger_account_id_fkey"}
	if got := paymentNotificationErrorCode(postgresError); got != "POSTGRES_23503_ledger_postings_ledger_account_id_fkey" {
		t.Fatalf("unexpected PostgreSQL notification error code %q", got)
	}
	if got := paymentNotificationErrorCode(ErrConflict); got != "BILLING_CONFLICT" {
		t.Fatalf("unexpected conflict notification error code %q", got)
	}
}
