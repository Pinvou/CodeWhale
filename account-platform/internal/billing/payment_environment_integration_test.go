package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
)

func TestPostgresTestOnlyCollectionEnvironmentBoundary(t *testing.T) {
	for _, environment := range []string{"", "development", "test", "staging", "production"} {
		t.Run(environment, func(t *testing.T) {
			ctx := context.Background()
			r := &PostgresRepository{pool: testdb.Open(t, "billing"), now: func() time.Time { return time.Now().UTC() }}
			wallet, _, err := r.EnsureWallet(ctx, "ORGANIZATION", uuid.New())
			if err != nil {
				t.Fatal(err)
			}
			mappings, err := r.ActiveExternalProducts(ctx, "development_simulator", "development-simulator-v1", true)
			if err != nil {
				t.Fatal(err)
			}
			provider := &legacyMappingProvider{}
			coordinator, err := NewPaymentCoordinatorWithProvider(r, provider, mappings, environment)
			if err != nil {
				t.Fatal("service constructor must permit test-only provider startup", err)
			}
			products, err := r.ListProducts(ctx, "CNY", "ALIPAY")
			if err != nil {
				t.Fatal(err)
			}
			development := environment == "" || environment == "development" || environment == "test"
			if coordinator.Capabilities().FormalCollectionAdmitted {
				t.Fatal("test-only provider advertised formal collection")
			}
			_, _, err = coordinator.CreateOrder(ctx, uuid.New(), wallet.ID, "CNY_1", "ALIPAY", uuid.NewString())
			if development {
				if err != nil || provider.creates != 1 {
					t.Fatal("development collection stopped working", err)
				}
			} else {
				if !errors.Is(err, ErrConflict) || provider.creates != 0 || len(coordinator.FilterProducts(products)) != 0 {
					t.Fatal("test-only formal collection escaped environment gate", err)
				}
				var orders int
				if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM payment_orders`).Scan(&orders); err != nil || orders != 0 {
					t.Fatal("rejected collection persisted an order", err)
				}
			}
			empty, err := NewPaymentCoordinatorWithProvider(r, provider, nil, environment)
			if err != nil || len(empty.FilterProducts(products)) != 0 {
				t.Fatal("empty catalog must start without advertising unavailable products", err)
			}
		})
	}
}

type controlledEnvironmentQueryProvider struct {
	*recoverableProvider
	reply ProviderOrder
}

func (p *controlledEnvironmentQueryProvider) Get(context.Context, string) (ProviderOrder, error) {
	return p.reply, nil
}
func TestPostgresProductionTestOnlyPendingQueryCannotCredit(t *testing.T) {
	ctx := context.Background()
	r, w := billingFixture(t)
	provider := &controlledEnvironmentQueryProvider{recoverableProvider: &recoverableProvider{}}
	mappings, err := r.ActiveExternalProducts(ctx, "development_simulator", "development-simulator-v1", true)
	if err != nil {
		t.Fatal(err)
	}
	dev, err := NewPaymentCoordinatorWithProvider(r, provider, mappings, "development")
	if err != nil {
		t.Fatal(err)
	}
	order, _, err := dev.CreateOrder(ctx, w.OwnerID, w.ID, "CNY_1", "ALIPAY", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now, tid := time.Now().UTC(), uuid.NewString()
	provider.reply = ProviderOrder{MerchantOrderNo: order.MerchantOrderNo, Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, Mobile: order.Mobile, ProviderSKU: order.ProviderSKU, Status: "SUCCESS", ChannelTransactionID: &tid, PaidAt: &now}
	prod, err := NewPaymentCoordinatorWithProvider(r, provider, mappings, "production")
	if err != nil {
		t.Fatal(err)
	}
	_, err = prod.SyncOrder(ctx, order.ID)
	wallet, e := r.GetWallet(ctx, w.ID)
	if e != nil {
		t.Fatal(e)
	}
	if !errors.Is(err, ErrConflict) || wallet.PostedMicrocredits != 0 {
		t.Fatalf("production test-only query posted formal credit: err=%v posted=%d", err, wallet.PostedMicrocredits)
	}
	if _, err := dev.SyncOrder(ctx, order.ID); err != nil {
		t.Fatal("development query stopped working", err)
	}
	credited, err := r.GetWallet(ctx, w.ID)
	if err != nil || credited.PostedMicrocredits != order.CreditMicrocredits {
		t.Fatal("development credit absent", err)
	}
	if _, err := prod.SyncOrder(ctx, order.ID); err != nil {
		t.Fatal("historical credited replay was rejected", err)
	}
	replay, err := r.GetWallet(ctx, w.ID)
	if err != nil || replay.PostedMicrocredits != credited.PostedMicrocredits {
		t.Fatal("credited replay altered historical funds", err)
	}
	var entries int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE entry_type='RECHARGE'`).Scan(&entries); err != nil || entries != 1 {
		t.Fatal("replay duplicated journal", err)
	}
}

func (p *controlledEnvironmentQueryProvider) Capabilities() PaymentCapabilities {
	caps := p.recoverableProvider.Capabilities()
	caps.ManualRepairSupported = true
	caps.ReviewLatePayment = true
	return caps
}
func TestPostgresProductionTestOnlyRepairCannotCredit(t *testing.T) {
	ctx := context.Background()
	r, w := billingFixture(t)
	provider := &controlledEnvironmentQueryProvider{recoverableProvider: &recoverableProvider{}}
	mappings, err := r.ActiveExternalProducts(ctx, "development_simulator", "development-simulator-v1", true)
	if err != nil {
		t.Fatal(err)
	}
	dev, err := NewPaymentCoordinatorWithProvider(r, provider, mappings, "development")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-time.Hour)
	dev.now = func() time.Time { return old }
	order, _, err := dev.CreateOrder(ctx, w.OwnerID, w.ID, "CNY_1", "ALIPAY", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now, tid := time.Now().UTC(), uuid.NewString()
	provider.reply = ProviderOrder{MerchantOrderNo: order.MerchantOrderNo, Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, Mobile: order.Mobile, ProviderSKU: order.ProviderSKU, Status: "SUCCESS", ChannelTransactionID: &tid, PaidAt: &now}
	dev.now = time.Now
	reviewed, err := dev.SyncOrder(ctx, order.ID)
	if err != nil || reviewed.Status != "MANUAL_REVIEW" {
		t.Fatal("late payment review fixture not reached", err)
	}
	prod, err := NewPaymentCoordinatorWithProvider(r, provider, mappings, "production")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prod.RepairReviewedOrder(ctx, order.ID, uuid.New(), "controlled review repair outside development"); !errors.Is(err, ErrConflict) {
		t.Fatal("production repaired TestOnly credit", err)
	}
	wallet, err := r.GetWallet(ctx, w.ID)
	if err != nil || wallet.PostedMicrocredits != 0 {
		t.Fatal("denied repair posted funds", err)
	}
	var repairs, journals int
	if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM payment_order_repairs),(SELECT count(*) FROM journal_entries)`).Scan(&repairs, &journals); err != nil || repairs != 0 || journals != 0 {
		t.Fatal("denied repair retained financial mutation", err)
	}
	if _, err := dev.RepairReviewedOrder(ctx, order.ID, uuid.New(), "controlled development review repair"); err != nil {
		t.Fatal("development audited repair stopped working", err)
	}
}
