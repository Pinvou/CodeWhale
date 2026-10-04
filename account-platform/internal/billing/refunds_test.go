package billing

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestProportionalCreditsRejectsInvalidOrFractionalValues(t *testing.T) {
	tests := []struct {
		name                    string
		orderFen, credits, part int64
		want                    int64
		ok                      bool
	}{
		{name: "full", orderFen: 100, credits: 1_000_000, part: 100, want: 1_000_000, ok: true},
		{name: "partial", orderFen: 100, credits: 1_000_000, part: 25, want: 250_000, ok: true},
		{name: "fractional", orderFen: 3, credits: 10, part: 1, ok: false},
		{name: "too much", orderFen: 100, credits: 1_000_000, part: 101, ok: false},
		{name: "zero", orderFen: 100, credits: 1_000_000, part: 0, ok: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := proportionalCredits(test.orderFen, test.credits, test.part)
			if got != test.want || ok != test.ok {
				t.Fatalf("proportionalCredits() = (%d, %v), want (%d, %v)", got, ok, test.want, test.ok)
			}
		})
	}
}

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
