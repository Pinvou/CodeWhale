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

func TestCompareFourWayReconciliationIncludesRefundAndLedgerDifferences(t *testing.T) {
	orderID := uuid.Must(uuid.NewV7())
	provider := []ProviderBillEntry{
		{EntryType: "PAYMENT", ProviderReferenceID: "payment-1", MerchantOrderNo: "order-1", AmountFen: 100, Currency: "CNY"},
		{EntryType: "REFUND", ProviderReferenceID: "refund-1", MerchantOrderNo: "order-1", AmountFen: 50, Currency: "CNY"},
	}
	internal := []ReconciliationInternalFact{
		{EntryType: "PAYMENT", ProviderReferenceID: "payment-1", MerchantOrderNo: "order-1", PaymentOrderID: orderID, AmountFen: 100, Currency: "CNY", LedgerPresent: false},
		{EntryType: "REFUND", ProviderReferenceID: "refund-1", MerchantOrderNo: "order-1", PaymentOrderID: orderID, AmountFen: 40, Currency: "CNY", LedgerPresent: true},
		{EntryType: "CHARGEBACK", ProviderReferenceID: "chargeback-1", MerchantOrderNo: "order-1", PaymentOrderID: orderID, AmountFen: 100, Currency: "CNY", LedgerPresent: true},
	}
	differences := CompareFourWayReconciliation(provider, internal)
	counts := map[string]int{}
	for _, difference := range differences {
		counts[difference.CaseType]++
	}
	for _, kind := range []string{"PAYMENT_LEDGER_MISSING", "REFUND_AMOUNT_MISMATCH", "INTERNAL_CHARGEBACK_PROVIDER_MISSING"} {
		if counts[kind] != 1 {
			t.Fatalf("expected one %s case: %#v", kind, counts)
		}
	}
}

func TestForcedRefundProportionalCeiling(t *testing.T) {
	tests := []struct {
		original, refund, payment, want int64
	}{
		{100, 50, 100, 50},
		{100, 1, 3, 34},
		{101, 2, 3, 68},
	}
	for _, item := range tests {
		got, err := proportionalCeiling(item.original, item.refund, item.payment)
		if err != nil || got != item.want {
			t.Fatalf("proportionalCeiling(%d,%d,%d) = %d, %v; want %d", item.original, item.refund, item.payment, got, err, item.want)
		}
	}
}

func TestReconciliationBusinessDateUsesShanghaiCalendarLabelAndRange(t *testing.T) {
	date := reconciliationBusinessDate(time.Date(2026, 8, 23, 1, 0, 0, 0, time.UTC))
	if got := date.Format("2006-01-02"); got != "2026-08-22" {
		t.Fatalf("business date = %s", got)
	}
	start, end := reconciliationBusinessDateRange(date)
	if start.UTC().Format(time.RFC3339) != "2026-08-21T16:00:00Z" || end.UTC().Format(time.RFC3339) != "2026-08-22T16:00:00Z" {
		t.Fatalf("unexpected Shanghai range %s - %s", start.UTC(), end.UTC())
	}
}
