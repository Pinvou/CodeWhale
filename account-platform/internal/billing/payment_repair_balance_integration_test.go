package billing

import (
	"context"
	"encoding/json"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/google/uuid"
	"testing"
	"time"
)

type controlledLateRepairProvider struct {
	legacyMappingProvider
	reply ProviderOrder
}

func (p *controlledLateRepairProvider) Capabilities() payment.Capabilities {
	c := p.legacyMappingProvider.Capabilities()
	c.ReviewLatePayment = true
	c.ManualRepairSupported = true
	return c
}
func (p *controlledLateRepairProvider) Create(_ context.Context, o payment.Order, _ string) (ProviderOrder, error) {
	tid := "controlled-manual-repair-" + o.MerchantOrderNo
	paid := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Microsecond)
	p.reply = ProviderOrder{MerchantOrderNo: o.MerchantOrderNo, Channel: o.Channel, AmountFen: o.AmountFen, Currency: o.Currency, Mobile: o.Mobile, ProviderSKU: o.ExternalSKU, Status: "SUCCESS", ChannelTransactionID: &tid, PaidAt: &paid}
	return p.reply, nil
}
func (p *controlledLateRepairProvider) Get(context.Context, string) (ProviderOrder, error) {
	return p.reply, nil
}
func TestPostgresRepairAuditRecordsNetWalletBalance(t *testing.T) {
	r, w, source := forcedRefundPaidSource(t)
	ctx := context.Background()
	hold, e := r.Reserve(ctx, uuid.New(), w.ID, w.PostedMicrocredits, nil, time.Now().Add(time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Commit(ctx, hold.UsageRequestID, hold.ID, w.PostedMicrocredits, map[string]any{"controlled_usage": true}); e != nil {
		t.Fatal(e)
	}
	review, e := r.RecordAndFreezeForcedRefund(ctx, source, "controlled-repair-debt", 100, json.RawMessage(`{"confirmed":true}`))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.ApplyReviewedForcedRefund(ctx, review.ID); e != nil {
		t.Fatal(e)
	}
	debt, e := r.GetWallet(ctx, w.ID)
	if e != nil || debt.ReceivableMicrocredits != w.PostedMicrocredits {
		t.Fatal("real debt absent", e)
	}
	mappings, e := r.ActiveExternalProducts(ctx, "development_simulator", "development-simulator-v1", true)
	if e != nil {
		t.Fatal(e)
	}
	provider := &controlledLateRepairProvider{}
	c, e := NewPaymentCoordinatorWithProvider(r, provider, mappings, "test")
	if e != nil {
		t.Fatal(e)
	}
	c.now = func() time.Time { return time.Now().UTC().Add(-time.Hour) }
	order, _, e := c.CreateOrder(ctx, uuid.New(), w.ID, "CNY_1", "ALIPAY", uuid.NewString())
	if e != nil || order.Status != "MANUAL_REVIEW" {
		t.Fatalf("late provider was not manual review status=%s err=%v", order.Status, e)
	}
	c.now = time.Now
	fixed, e := c.RepairReviewedOrder(ctx, order.ID, uuid.New(), "controlled manual repair with prior debt")
	if e != nil || fixed.FulfillmentStatus != "CREDITED" {
		t.Fatalf("repair failed %v", e)
	}
	actual, e := r.GetWallet(ctx, w.ID)
	if e != nil {
		t.Fatal(e)
	}
	var before, after int64
	if e = r.pool.QueryRow(ctx, `SELECT wallet_balance_before,wallet_balance_after FROM payment_order_repairs WHERE payment_order_id=$1`, order.ID).Scan(&before, &after); e != nil {
		t.Fatal(e)
	}
	t.Logf("audit before=%d audit after=%d actual=%d debt=%d", before, after, actual.PostedMicrocredits, actual.ReceivableMicrocredits)
	if actual.ReceivableMicrocredits != 0 {
		t.Fatal("BASE did not clear real debt")
	}
	if after != actual.PostedMicrocredits {
		t.Fatal("repair audit used gross credit rather than actual net wallet balance")
	}
}
