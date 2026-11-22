package billing

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

type systemLedgerAccountExpectation struct {
	ID          uuid.UUID
	Code        string
	AccountType string
}

var systemLedgerAccountExpectations = []systemLedgerAccountExpectation{
	{ID: paymentClearingAccount, Code: "platform:payment-clearing", AccountType: "PAYMENT_CLEARING"},
	{ID: usageRevenueAccount, Code: "platform:usage-revenue", AccountType: "USAGE_REVENUE"},
	{ID: refundClearingAccount, Code: "platform:refund-clearing", AccountType: "REFUND_CLEARING"},
	{ID: adjustmentAccount, Code: "platform:adjustment", AccountType: "ADJUSTMENT"},
	{ID: receivableAccount, Code: "platform:receivables", AccountType: "RECEIVABLE_ASSET"},
	{ID: chargebackClearingAccount, Code: "platform:chargeback-clearing", AccountType: "CHARGEBACK_CLEARING"},
}

type systemLedgerAccountActual struct {
	Code        string
	AccountType string
	WalletID    *uuid.UUID
}

// ValidateSystemLedgerAccounts prevents Billing from accepting traffic when a
// data cleanup has removed or altered platform-owned financial accounts.
func (r *PostgresRepository) ValidateSystemLedgerAccounts(ctx context.Context) error {
	rows, err := r.pool.Query(ctx, `SELECT id,code,account_type,wallet_id FROM ledger_accounts WHERE id IN ($1,$2,$3,$4,$5,$6)`,
		systemLedgerAccountExpectations[0].ID,
		systemLedgerAccountExpectations[1].ID,
		systemLedgerAccountExpectations[2].ID,
		systemLedgerAccountExpectations[3].ID,
		systemLedgerAccountExpectations[4].ID,
		systemLedgerAccountExpectations[5].ID,
	)
	if err != nil {
		return fmt.Errorf("query billing system ledger accounts: %w", err)
	}
	defer rows.Close()
	actual := make(map[uuid.UUID]systemLedgerAccountActual, len(systemLedgerAccountExpectations))
	for rows.Next() {
		var id uuid.UUID
		var item systemLedgerAccountActual
		if err := rows.Scan(&id, &item.Code, &item.AccountType, &item.WalletID); err != nil {
			return fmt.Errorf("scan billing system ledger account: %w", err)
		}
		actual[id] = item
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read billing system ledger accounts: %w", err)
	}
	return validateSystemLedgerAccountSet(actual)
}

func validateSystemLedgerAccountSet(actual map[uuid.UUID]systemLedgerAccountActual) error {
	for _, expected := range systemLedgerAccountExpectations {
		item, ok := actual[expected.ID]
		if !ok {
			return fmt.Errorf("billing system ledger account %s is missing", expected.Code)
		}
		if item.WalletID != nil || item.Code != expected.Code || item.AccountType != expected.AccountType {
			return fmt.Errorf("billing system ledger account %s has invalid immutable attributes", expected.Code)
		}
	}
	return nil
}
