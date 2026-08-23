package billing

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCompareReconciliationFindsRequiredDifferenceClasses(t *testing.T) {
	paidAt := time.Date(2026, 8, 12, 1, 0, 0, 0, time.UTC)
	internalA := PaymentOrder{ID: uuid.Must(uuid.NewV7()), MerchantOrderNo: "internal-only", AmountFen: 100, Currency: "CNY"}
	internalB := PaymentOrder{ID: uuid.Must(uuid.NewV7()), MerchantOrderNo: "amount-mismatch", AmountFen: 100, Currency: "CNY"}
	provider := []ProviderBillEntry{
		{MerchantOrderNo: "provider-only", ChannelTransactionID: "tx-1", AmountFen: 100, Currency: "CNY", PaidAt: paidAt},
		{MerchantOrderNo: "amount-mismatch", ChannelTransactionID: "tx-2", AmountFen: 101, Currency: "CNY", PaidAt: paidAt},
		{MerchantOrderNo: "duplicate-a", ChannelTransactionID: "tx-duplicate", AmountFen: 100, Currency: "CNY", PaidAt: paidAt},
		{MerchantOrderNo: "duplicate-b", ChannelTransactionID: "tx-duplicate", AmountFen: 100, Currency: "CNY", PaidAt: paidAt},
	}
	differences := CompareReconciliation(provider, []PaymentOrder{internalA, internalB})
	counts := map[string]int{}
	for _, difference := range differences {
		counts[difference.CaseType]++
	}
	for _, kind := range []string{"PROVIDER_PAID_INTERNAL_MISSING", "INTERNAL_PAID_PROVIDER_MISSING", "AMOUNT_MISMATCH", "DUPLICATE_TRANSACTION"} {
		if counts[kind] == 0 {
			t.Fatalf("missing reconciliation case %s: %#v", kind, counts)
		}
	}
}
