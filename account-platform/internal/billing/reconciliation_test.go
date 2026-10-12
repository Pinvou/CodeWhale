package billing

import (
	"github.com/google/uuid"
	"testing"
	"time"
)

func TestCompareFourWayReconciliationIncludesRefundAndLedgerDifferences(t *testing.T) {
	orderID := uuid.Must(uuid.NewV7())
	provider := []ProviderBillEntry{
		{EntryType: "PAYMENT", ProviderReferenceID: "payment-1", MerchantOrderNo: "order-1", AmountFen: 100, Currency: "CNY", Status: "SUCCESS"},
		{EntryType: "REFUND", ProviderReferenceID: "refund-1", MerchantOrderNo: "order-1", AmountFen: 50, Currency: "CNY", Status: "SUCCESS"},
	}
	internal := []ReconciliationInternalFact{
		{EntryType: "PAYMENT", ProviderReferenceID: "payment-1", MerchantOrderNo: "order-1", PaymentOrderID: orderID, AmountFen: 100, Currency: "CNY", LedgerPresent: false},
		{EntryType: "REFUND", ProviderReferenceID: "refund-1", MerchantOrderNo: "order-1", PaymentOrderID: orderID, AmountFen: 40, Currency: "CNY", LedgerPresent: true},
		{EntryType: "CHARGEBACK", ProviderReferenceID: "chargeback-1", EventDateVerified: true, MerchantOrderNo: "order-1", PaymentOrderID: orderID, AmountFen: 100, Currency: "CNY", LedgerPresent: true},
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

func TestFourWayUnsuccessfulProviderRowDoesNotHideMissingPaidFact(t *testing.T) {
	provider := []ProviderBillEntry{{EntryType: "PAYMENT", ProviderReferenceID: "payment-1", MerchantOrderNo: "order-1", AmountFen: 100, Currency: "CNY", Status: "FAILED"}}
	internal := []ReconciliationInternalFact{{EntryType: "PAYMENT", ProviderReferenceID: "payment-1", MerchantOrderNo: "order-1", PaymentOrderID: uuid.New(), AmountFen: 100, Currency: "CNY", LedgerPresent: true}}
	counts := map[string]int{}
	for _, difference := range CompareFourWayReconciliation(provider, internal) {
		counts[difference.CaseType]++
	}
	if counts["UNSUPPORTED_PROVIDER_ENTRY"] != 1 || counts["INTERNAL_PAID_PROVIDER_MISSING"] != 1 {
		t.Fatal("failed row hid missing confirmed provider payment", counts)
	}
}

func TestF03FourWayMerchantOwnership(t *testing.T) {
	for _, entry := range []string{"PAYMENT", "REFUND", "CHARGEBACK"} {
		t.Run(entry, func(t *testing.T) {
			row := ProviderBillEntry{EntryType: entry, ProviderReferenceID: "controlled-reference", ChannelTransactionID: "controlled-reference", MerchantOrderNo: "other-order", AmountFen: 100, Currency: "CNY", Status: "SUCCESS"}
			fact := ReconciliationInternalFact{EntryType: entry, EventDateVerified: true, ProviderReferenceID: "controlled-reference", MerchantOrderNo: "actual-order", AmountFen: 100, Currency: "CNY", LedgerPresent: true}
			prefix := map[string]string{"PAYMENT": "PAID", "REFUND": "REFUND", "CHARGEBACK": "CHARGEBACK"}[entry]
			counts := map[string]int{}
			for _, difference := range CompareFourWayReconciliation([]ProviderBillEntry{row}, []ReconciliationInternalFact{fact}) {
				counts[difference.CaseType]++
			}
			if len(counts) != 2 || counts["INTERNAL_"+prefix+"_PROVIDER_MISSING"] != 1 || counts["PROVIDER_"+prefix+"_INTERNAL_MISSING"] != 1 {
				t.Fatal("cross-order reference hid both missing sides", counts)
			}
			row.MerchantOrderNo = fact.MerchantOrderNo
			if differences := CompareFourWayReconciliation([]ProviderBillEntry{row}, []ReconciliationInternalFact{fact}); len(differences) != 0 {
				t.Fatal("matching order/reference no longer reconciles")
			}
			other := row
			other.MerchantOrderNo = "another-order"
			counts = map[string]int{}
			for _, difference := range CompareFourWayReconciliation([]ProviderBillEntry{row, other}, []ReconciliationInternalFact{fact}) {
				counts[difference.CaseType]++
			}
			if len(counts) != 1 || counts["DUPLICATE_TRANSACTION"] != 1 {
				t.Fatal("cross-order duplicate reference detection changed", counts)
			}
		})
	}
}
