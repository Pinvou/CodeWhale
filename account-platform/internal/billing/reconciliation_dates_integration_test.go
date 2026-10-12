package billing

import (
	"github.com/google/uuid"
	"testing"
	"time"
)

func TestF03ReversalOccurrenceDayDoesNotBecomeSettlementDay(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	original, order, transaction, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
	original.AllowNormalRefundCreation(true)
	refund, _, err := original.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "controlled cross-day refund")
	if err != nil {
		t.Fatal(err)
	}
	reference := "controlled-cross-day-refund"
	if _, _, err := original.finishRefund(ctx, refund, ProviderRefund{MerchantRefundNo: refund.MerchantRefundNo, AmountFen: refund.AmountFen, Status: "SUCCESS", ChannelRefundID: reference}, false); err != nil {
		t.Fatal(err)
	}
	now := r.now()
	previous := normalizedReconciliationBusinessDate(now).AddDate(0, 0, -1)
	supplierOccurrence := previous.Add(4 * time.Hour)
	terminal := original.provider.(*retainedFinancialProvider).result
	paymentRow := ProviderBillEntry{EntryType: "PAYMENT", ProviderReferenceID: *terminal.ChannelTransactionID, ChannelTransactionID: *terminal.ChannelTransactionID, MerchantOrderNo: order.MerchantOrderNo, Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, Status: "SUCCESS", PaidAt: *terminal.PaidAt}
	start, end := reconciliationBusinessDateRange(normalizedReconciliationBusinessDate(now))
	facts, err := original.listFourWayReconciliationFacts(ctx, r.pool, order.Channel, start, end)
	if err != nil {
		t.Fatal(err)
	}
	dateUnknown, wrongMissing := false, false
	for _, difference := range CompareFourWayReconciliation([]ProviderBillEntry{paymentRow}, facts) {
		dateUnknown = dateUnknown || difference.CaseType == "REFUND_DATE_UNVERIFIED"
		wrongMissing = wrongMissing || difference.CaseType == "INTERNAL_REFUND_PROVIDER_MISSING"
	}
	if !dateUnknown || wrongMissing {
		t.Fatal("unverified supplier date reported as missing money")
	}
	provider := &reconciliationFixtureProvider{retainedFinancialProvider: *original.provider.(*retainedFinancialProvider), rows: []ProviderBillEntry{{EntryType: "REFUND", ProviderReferenceID: reference, ChannelTransactionID: reference, MerchantOrderNo: order.MerchantOrderNo, Channel: order.Channel, AmountFen: refund.AmountFen, Currency: order.Currency, Status: "SUCCESS", PaidAt: supplierOccurrence}}}
	c, err := NewPaymentCoordinatorWithProvider(r, provider, "test")
	if err != nil {
		t.Fatal(err)
	}
	first, err := c.RunAdminReconciliation(ctx, uuid.New(), order.Channel, previous)
	if err != nil || first.CaseCount != 0 {
		t.Fatal("cross-day verified refund falsely missing", first.CaseCount, err)
	}
	provider.rows = []ProviderBillEntry{paymentRow}
	today, err := c.RunAdminReconciliation(ctx, uuid.New(), order.Channel, now)
	if err != nil || today.CaseCount != 0 {
		t.Fatal("settlement day falsely duplicates supplier occurrence", today.CaseCount, err)
	}
	provider.rows = []ProviderBillEntry{{EntryType: "REFUND", ProviderReferenceID: reference, ChannelTransactionID: reference, MerchantOrderNo: "different-order", Channel: order.Channel, AmountFen: refund.AmountFen, Currency: order.Currency, Status: "SUCCESS", PaidAt: supplierOccurrence}}
	wrong, err := c.RunAdminReconciliation(ctx, uuid.New(), order.Channel, previous)
	if err != nil || wrong.CaseCount != 1 {
		t.Fatal("cross-day supplement accepted wrong merchant", wrong.CaseCount, err)
	}
	var posted, journals int64
	if err := r.pool.QueryRow(ctx, `SELECT posted_microcredits,(SELECT count(*) FROM journal_entries) FROM wallets WHERE id=$1`, order.WalletID).Scan(&posted, &journals); err != nil || posted != 0 || journals != 2 {
		t.Fatal("date reconciliation modified financial state", posted, journals, err)
	}
}
