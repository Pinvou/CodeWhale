package billing

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/google/uuid"
	"strings"
	"testing"
	"time"
)

// Uses real PostgreSQL migrations, repository, payment projection and ledger.
// Only the provider's terminal response is controlled; no live vendor is used.
func TestRetainedPaymentTerminalQueryPreservesPaidFacts(t *testing.T) {
	for _, status := range []string{"FAILED", "CLOSED"} {
		for _, paid := range []bool{true, false} {
			name := status + "/unpaid"
			if paid {
				name = status + "/paid"
			}
			t.Run(name, func(t *testing.T) {
				r, ctx := retainedFinanceRepository(t)
				c, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, paid)
				provider := c.provider.(*retainedFinancialProvider)
				c.provider = &authoritativeTerminalProvider{provider}
				provider.result.Status = status
				provider.result.ChannelTransactionID, provider.result.PaidAt = nil, nil
				before, err := r.GetWallet(ctx, order.WalletID)
				if err != nil {
					t.Fatal(err)
				}
				got, err := c.SyncOrder(ctx, order.ID)
				if err != nil {
					t.Fatal(err)
				}
				after, err := r.GetWallet(ctx, order.WalletID)
				if err != nil {
					t.Fatal(err)
				}
				expectedStatus, expectedFulfillment := status, "PENDING"
				if paid {
					expectedStatus, expectedFulfillment = "PAID", "CREDITED"
				}
				if got.Status != expectedStatus || got.FulfillmentStatus != expectedFulfillment ||
					before.PostedMicrocredits != after.PostedMicrocredits || before.ReservedMicrocredits != after.ReservedMicrocredits {
					t.Fatalf("terminal query changed payment facts: status=%s fulfillment=%s moneyExact=%v", got.Status, got.FulfillmentStatus, before.PostedMicrocredits == after.PostedMicrocredits && before.ReservedMicrocredits == after.ReservedMicrocredits)
				}
				if paid && got.CloseStatus != order.CloseStatus {
					t.Fatal("stale unpaid observation changed paid close state")
				}
				var journals int
				if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE entry_type='RECHARGE'`).Scan(&journals); err != nil {
					t.Fatal(err)
				}
				expectedJournals := 0
				if paid {
					expectedJournals = 1
				}
				if journals != expectedJournals {
					t.Fatal("terminal query changed recharge ledger count")
				}
			})
		}
	}
}

type authoritativeTerminalProvider struct{ *retainedFinancialProvider }

func (p *authoritativeTerminalProvider) Capabilities() PaymentCapabilities {
	capabilities := p.retainedFinancialProvider.Capabilities()
	capabilities.AuthoritativeUnpaidTerminal = true
	return capabilities
}

// Controlled Create response waits for a real successful payment merge before
// returning. No SQL status/ledger fabrication or real supplier is used.
func TestRetainedCreateResponsePreservesConfirmedPayment(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	owner := uuid.New()
	wallet, _, e := r.EnsureWallet(ctx, "PERSONAL", owner)
	if e != nil {
		t.Fatal(e)
	}
	input := validRechargeProductForEditTests()
	input.ProductID = "RACE_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	input.Mapping.ProviderID = "controlled"
	input.Mapping.ExternalSKUBOM = input.ProductID
	item, e := r.CreateRechargeProduct(ctx, input)
	if e != nil {
		t.Fatal(e)
	}
	verification, _, e := r.StartProductAdmissionVerification(ctx, item.CatalogItemID, owner, nil)
	if e != nil {
		t.Fatal(e)
	}
	evidence, e := r.CompleteProductAdmissionVerification(ctx, verification.VerificationID, "controlled", "controlled-admission", "private-probe", time.Now().Add(time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	_, e = r.TransitionRechargeProduct(ctx, item.CatalogItemID, item.Version, "ON_SALE", &evidence)
	if e != nil {
		t.Fatal(e)
	}
	provider := &retainedCreateRaceProvider{retainedFinancialProvider: &retainedFinancialProvider{}}
	c, e := NewPaymentCoordinatorWithProvider(r, provider, "test")
	if e != nil {
		t.Fatal(e)
	}
	c.AllowTentativePrices(true)
	provider.coordinator = c
	returned, _, e := c.CreateOrder(ctx, owner, wallet.ID, input.ProductID, "ALIPAY", "controlled-create-race-key")
	if e != nil {
		t.Fatal(e)
	}
	stored, e := getPaymentOrderByID(ctx, r.pool, returned.ID, false)
	if e != nil {
		t.Fatal(e)
	}
	if stored.Status != "PAID" || stored.FulfillmentStatus != "CREDITED" || returned.Status != "PAID" || returned.FulfillmentStatus != "CREDITED" {
		t.Fatalf("create response replaced confirmed payment: stored=%s/%s returned=%s/%s", stored.Status, stored.FulfillmentStatus, returned.Status, returned.FulfillmentStatus)
	}
	replay, _, e := c.CreateOrder(ctx, owner, wallet.ID, input.ProductID, "ALIPAY", "controlled-create-race-key")
	if e != nil || replay.Status != "PAID" || replay.FulfillmentStatus != "CREDITED" {
		t.Fatal("receipt replay lost confirmed payment", e)
	}
	if stored.PaymentURL != "" || stored.QRData != "" || returned.PaymentURL != "" || returned.QRData != "" || replay.PaymentURL != "" || replay.QRData != "" {
		t.Fatal("confirmed payment exposed stale payment instructions")
	}
	balance, e := r.GetWallet(ctx, wallet.ID)
	if e != nil || balance.PostedMicrocredits != returned.CreditMicrocredits {
		t.Fatal("confirmed payment wallet amount changed", e)
	}
	var count int
	if e = r.pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE entry_type='RECHARGE'`).Scan(&count); e != nil || count != 1 {
		t.Fatal("recharge ledger count changed", e)
	}
}

type retainedCreateRaceProvider struct {
	*retainedFinancialProvider
	coordinator *PaymentCoordinator
}

func (p *retainedCreateRaceProvider) Create(ctx context.Context, input payment.Order, _ string) (ProviderOrder, error) {
	var id uuid.UUID
	if e := p.coordinator.repository.pool.QueryRow(ctx, `SELECT id FROM payment_orders WHERE merchant_order_no=$1`, input.MerchantOrderNo).Scan(&id); e != nil {
		return ProviderOrder{}, e
	}
	now := time.Now().UTC()
	transaction := uuid.NewString()
	fact := ProviderOrder{MerchantOrderNo: input.MerchantOrderNo, ExternalCustomerKey: input.MerchantCustomerKey, ExternalSKU: input.ExternalSKU, Channel: input.Channel, AmountFen: input.AmountFen, Currency: input.Currency, Status: "SUCCESS", ChannelTransactionID: &transaction, PaidAt: &now}
	applied, e := p.coordinator.applyQueriedPayment(ctx, id, fact, false)
	if e != nil {
		return ProviderOrder{}, e
	}
	if applied.Status != "PAID" || applied.FulfillmentStatus != "CREDITED" {
		return ProviderOrder{}, errors.New("controlled successful query did not credit")
	}
	return ProviderOrder{MerchantOrderNo: input.MerchantOrderNo, Channel: input.Channel, AmountFen: input.AmountFen, Currency: input.Currency, ExternalCustomerKey: input.MerchantCustomerKey, ExternalSKU: input.ExternalSKU, Status: "PENDING", PaymentURL: "https://provider.invalid/pay"}, nil
}
