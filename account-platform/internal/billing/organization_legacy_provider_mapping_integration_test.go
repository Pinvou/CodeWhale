package billing

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
)

// This adapter returns controlled provider facts. The coordinator, mapping
// catalog, order, payment transaction and component settlement are production
// paths on PostgreSQL; no external supplier or environment startup is tested.
type legacyMappingProvider struct{ creates int }

func (*legacyMappingProvider) Capabilities() payment.Capabilities {
	return payment.Capabilities{ProviderID: "development_simulator", Version: "development-simulator-v1", Channels: []string{"ALIPAY", "WECHAT", "UNIONPAY"}, OrderQuerySupported: true, MinimumQueryIntervalSeconds: 1, MaximumPayableWindowSeconds: 900, MaximumLateConfirmationSeconds: 259200, OrderHistoryRetentionSeconds: 2592000, TestOnly: true}
}

func TestPostgresConfirmedChargebackSummaryMoneyWire(t *testing.T) {
	ctx := context.Background()
	repo := &PostgresRepository{pool: testdb.Open(t, "billing"), now: func() time.Time { return time.Now().UTC() }}
	user, organization := uuid.New(), uuid.New()
	wallet, _, err := repo.EnsureWallet(ctx, "ORGANIZATION", organization)
	if err != nil {
		t.Fatal(err)
	}
	mappings, err := repo.ActiveExternalProducts(ctx, "development_simulator", "development-simulator-v1", true)
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewPaymentCoordinatorWithProvider(repo, &legacyMappingProvider{}, mappings, "test")
	if err != nil {
		t.Fatal(err)
	}
	order, _, err := coordinator.CreateOrder(ctx, user, wallet.ID, "CNY_1", "ALIPAY", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	var transaction uuid.UUID
	if err := repo.pool.QueryRow(ctx, `SELECT id FROM payment_transactions WHERE payment_order_id=$1`, order.ID).Scan(&transaction); err != nil {
		t.Fatal(err)
	}
	chargeback, err := repo.RecordConfirmedChargebackForReview(ctx, transaction, "controlled-confirmed-chargeback", json.RawMessage(`{"confirmed":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ApplyConfirmedChargeback(ctx, chargeback.PaymentEvidenceID, "controlled-confirmed-chargeback"); err != nil {
		t.Fatal(err)
	}
	detail, err := repo.AdminPaymentOrder(ctx, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	var summary map[string]json.RawMessage
	if err := json.Unmarshal(detail.ChargebackSummary, &summary); err != nil {
		t.Fatal(err)
	}
	if string(summary["lost_amount_fen"]) != `"100"` || string(summary["open_count"]) != "1" {
		t.Fatalf("confirmed chargeback amount/count wire lost: %s", detail.ChargebackSummary)
	}
}
func (p *legacyMappingProvider) Create(_ context.Context, order payment.Order, _ string) (payment.ProviderOrder, error) {
	p.creates++
	now := time.Now().UTC().Truncate(time.Microsecond)
	transaction := "controlled-transaction-" + order.MerchantOrderNo
	return payment.ProviderOrder{MerchantOrderNo: order.MerchantOrderNo, Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, Mobile: order.Mobile, ProviderSKU: order.ExternalSKU, Status: "SUCCESS", ChannelTransactionID: &transaction, PaidAt: &now}, nil
}
func (*legacyMappingProvider) Get(context.Context, string) (payment.ProviderOrder, error) {
	return payment.ProviderOrder{}, payment.ErrUnsupported
}
func (*legacyMappingProvider) Close(context.Context, string) (payment.ProviderOrder, error) {
	return payment.ProviderOrder{}, payment.ErrUnsupported
}
func (*legacyMappingProvider) Refund(context.Context, string, string, int64) (payment.Refund, error) {
	return payment.Refund{}, payment.ErrUnsupported
}
func (*legacyMappingProvider) Bill(context.Context, string, time.Time) ([]payment.BillEntry, error) {
	return nil, payment.ErrUnsupported
}

func TestPostgresOrganizationLegacyRechargeUsesExistingCatalogAndProviderMapping(t *testing.T) {
	for productID, amount := range map[string]int64{"CNY_1": 100, "CNY_10": 1000, "CNY_100": 10000, "CNY_200": 20000} {
		t.Run(productID, func(t *testing.T) {
			ctx := context.Background()
			repo := &PostgresRepository{pool: testdb.Open(t, "billing"), now: func() time.Time { return time.Now().UTC() }}
			organization, user := uuid.New(), uuid.New()
			wallet, _, err := repo.EnsureWallet(ctx, "ORGANIZATION", organization)
			if err != nil {
				t.Fatal(err)
			}
			mappings, err := repo.ActiveExternalProducts(ctx, "development_simulator", "development-simulator-v1", true)
			if err != nil {
				t.Fatal(err)
			}
			mapping := mappings[productID]
			if mapping.MappingID == uuid.Nil || mapping.AmountFen != amount || mapping.Currency != "CNY" || mapping.Status != "TEST_ONLY" {
				t.Fatal("existing legacy catalog facts were not preserved in the new mapping")
			}
			// Original A10 seeded only PLAN mappings. Reproduce that catalog
			// omission without modifying database or existing product facts.
			planOnly := make(map[string]ExternalProduct)
			for id, value := range mappings {
				if id != "CNY_1" && id != "CNY_10" && id != "CNY_100" && id != "CNY_200" {
					planOnly[id] = value
				}
			}
			provider := &legacyMappingProvider{}
			before, err := NewPaymentCoordinatorWithProvider(repo, provider, planOnly, "test")
			if err != nil {
				t.Fatal(err)
			}
			key := uuid.NewString()
			if _, _, err := before.CreateOrder(ctx, user, wallet.ID, productID, "ALIPAY", key); !errors.Is(err, ErrConflict) || provider.creates != 0 {
				t.Fatalf("missing mapping did not reject before provider side effects: %v", err)
			}
			coordinator, err := NewPaymentCoordinatorWithProvider(repo, provider, mappings, "test")
			if err != nil {
				t.Fatal(err)
			}
			order, created, err := coordinator.CreateOrder(ctx, user, wallet.ID, productID, "ALIPAY", key)
			if err != nil || !created || order.Status != "PAID" || order.FulfillmentStatus != "CREDITED" || order.AmountFen != amount || order.CreditMicrocredits != amount*10000 {
				t.Fatalf("existing organization recharge did not settle: %v", err)
			}
			replay, created, err := coordinator.CreateOrder(ctx, user, wallet.ID, productID, "ALIPAY", key)
			if err != nil || created || replay.ID != order.ID || provider.creates != 1 {
				t.Fatalf("lost acknowledgement repeated payment or changed order: %v", err)
			}
			packages, summary, err := repo.ListOrganizationPackages(ctx, organization, wallet.ID, 10)
			if err != nil || len(packages) != 1 || packages[0].PaymentOrderID == nil || *packages[0].PaymentOrderID != order.ID || packages[0].ValidUntil != nil || packages[0].PointsTotal != amount*10000 || packages[0].PointsRemaining != amount*10000 || summary.AvailableMicrocredits != strconv.FormatInt(amount*10000, 10) {
				t.Fatalf("legacy no-expiry component entitlement lost: %v", err)
			}
			var lots, transactions, entries int
			var refundable bool
			var expiry *time.Time
			var remaining, posted int64
			if err := repo.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM credit_lots WHERE wallet_id=$1),(SELECT count(*) FROM payment_transactions WHERE payment_order_id=$2),(SELECT count(*) FROM journal_entries WHERE business_reference LIKE 'payment-transaction:%'),lot.refundable,lot.expires_at,lot.remaining_microcredits,wallet.posted_microcredits FROM credit_lots lot JOIN wallets wallet ON wallet.id=lot.wallet_id WHERE wallet.id=$1`, wallet.ID, order.ID).Scan(&lots, &transactions, &entries, &refundable, &expiry, &remaining, &posted); err != nil || lots != 1 || transactions != 1 || entries != 1 || !refundable || expiry != nil || remaining != amount*10000 || posted != remaining {
				t.Fatalf("legacy financial facts duplicated or changed: %v", err)
			}
		})
	}
}
