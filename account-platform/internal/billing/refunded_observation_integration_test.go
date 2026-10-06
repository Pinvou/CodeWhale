package billing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/paymentsim"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/paymentsimulator"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresHTTPSimulatorFirstRefundObservationRequiresReviewAndLateSuccessCannotCredit(t *testing.T) {
	for _, kind := range []string{"partial", "full"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			p, w, user := paymentFixture(t)
			pool := testdb.Open(t, "payment-simulator")
			dsn, err := url.Parse(pool.Config().ConnString())
			if err != nil {
				t.Fatal(err)
			}
			q := dsn.Query()
			q.Set("search_path", pool.Config().ConnConfig.RuntimeParams["search_path"])
			dsn.RawQuery = q.Encode()
			var app http.Handler
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { app.ServeHTTP(w, r) }))
			defer server.Close()
			secret := strings.Repeat("s", 32)
			sim, err := paymentsim.NewPostgresRepository(ctx, dsn.String(), server.URL, strings.Repeat("t", 32), map[string]string{"UNIONPAY": secret, "ALIPAY": secret, "WECHAT": secret})
			if err != nil {
				t.Fatal(err)
			}
			defer sim.Close()
			app = paymentsim.NewService(sim).Handler(secret)
			provider, err := paymentsimulator.New(server.URL, secret)
			if err != nil {
				t.Fatal(err)
			}
			p.provider = provider
			var product string
			if err = p.repository.pool.QueryRow(ctx, `SELECT product_id FROM recharge_products WHERE price_status='LEGACY' AND enabled ORDER BY amount_fen LIMIT 1`).Scan(&product); err != nil {
				t.Fatal(err)
			}
			order, _, err := p.CreateOrder(ctx, user, w.ID, product, "ALIPAY", uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			response, err := http.Post(order.PaymentURL+"/complete", "application/x-www-form-urlencoded", nil)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != 200 {
				t.Fatal("simulated payment did not complete")
			}
			amount := order.AmountFen
			if kind == "partial" {
				amount /= 2
			}
			if _, err = provider.Refund(ctx, order.MerchantOrderNo, uuid.NewString(), amount); err != nil {
				t.Fatal(err)
			}
			fact, err := sim.GetByMerchantOrder(ctx, order.MerchantOrderNo)
			if err != nil || fact.RefundedAmountFen != amount {
				t.Fatal("real simulator refund fact missing")
			}
			order, err = p.SyncOrder(ctx, order.ID)
			if err != nil || order.Status != "MANUAL_REVIEW" || order.FulfillmentStatus != "MANUAL_REVIEW" {
				t.Fatal("uncredited refunded observation did not enter review")
			}
			prior, err := provider.Get(ctx, order.MerchantOrderNo)
			if err != nil {
				t.Fatal(err)
			}
			prior.Status = "SUCCESS"
			if _, err = p.applyQueriedPayment(ctx, order.ID, prior, false); err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(map[string]any{"notification_id": uuid.New(), "merchant_id": p.merchantID, "merchant_order_no": order.MerchantOrderNo, "channel_transaction_id": *prior.ChannelTransactionID, "channel": order.Channel, "status": "SUCCESS", "amount_fen": order.AmountFen, "currency": order.Currency, "paid_at": prior.PaidAt, "timestamp": p.now().Unix()})
			for range 2 {
				if err = p.ProcessNotification(ctx, order.Channel, paymentSignature(p.secrets[order.Channel], body), body); err != nil {
					t.Fatal(err)
				}
			}
			wallet, err := p.repository.GetWallet(ctx, w.ID)
			if err != nil || wallet.PostedMicrocredits != 0 || wallet.ReservedMicrocredits != 0 {
				t.Fatal("review was penetrated by delayed SUCCESS")
			}
			var credits, events int
			if err = p.repository.pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE business_reference=$1`, "recharge:"+order.ID.String()).Scan(&credits); err != nil {
				t.Fatal(err)
			}
			if err = p.repository.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id=$1`, order.ID).Scan(&events); err != nil || credits != 0 || events != 0 {
				t.Fatal("refunded first observation emitted a credit or recharge event")
			}
			assertBalanced(t, p.repository)
		})
	}
}

type concurrentRefundNotificationProvider struct {
	*recoverableProvider
	coordinator *PaymentCoordinator
	appName     string
	delivery    chan error
	createCalls int
}

func (p *concurrentRefundNotificationProvider) Create(ctx context.Context, order payment.Order, callback string) (ProviderOrder, error) {
	p.createCalls++
	paidAt := p.coordinator.now()
	transaction := uuid.NewString()
	payload, _ := json.Marshal(map[string]any{"notification_id": uuid.New(), "merchant_id": p.coordinator.merchantID, "merchant_order_no": order.MerchantOrderNo, "channel_transaction_id": transaction, "channel": order.Channel, "status": "SUCCESS", "amount_fen": order.AmountFen, "currency": order.Currency, "paid_at": paidAt, "timestamp": paidAt.Unix()})
	go func() {
		request, err := http.NewRequest("POST", callback, strings.NewReader(string(payload)))
		if err != nil {
			p.delivery <- err
			return
		}
		request.Header.Set("X-Pinvou-Payment-Channel", order.Channel)
		request.Header.Set("X-Pinvou-Payment-Signature", paymentSignature(p.coordinator.secrets[order.Channel], payload))
		client := &http.Client{Timeout: 10 * time.Second}
		response, err := client.Do(request)
		if err != nil {
			p.delivery <- err
			return
		}
		defer response.Body.Close()
		io.Copy(io.Discard, response.Body)
		if response.StatusCode != 200 {
			p.delivery <- ErrConflict
			return
		}
		p.delivery <- nil
	}()
	// Prove that the actual notification HTTP request has reached the wallet
	// row gate while Create still owns it. No scheduler sleep assumes the race.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		err := p.coordinator.repository.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock' AND query LIKE '%wallets%FOR UPDATE%')`, p.appName).Scan(&waiting)
		if err != nil {
			return ProviderOrder{}, err
		}
		if waiting {
			return ProviderOrder{MerchantOrderNo: order.MerchantOrderNo, Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, Mobile: order.Mobile, ProviderSKU: order.ExternalSKU, Status: "REFUNDED", PaymentURL: p.coordinator.callbackURL + "/pay"}, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return ProviderOrder{}, errors.New("controlled notification did not reach the held order lock")
}

func TestPostgresHTTPCreateRefundFactCommitsReviewBeforeBlockedSuccessNotification(t *testing.T) {
	ctx := context.Background()
	p, w, user := paymentFixture(t)
	config := p.repository.pool.Config()
	appName := "owned-create-refund-" + uuid.NewString()
	config.ConnConfig.RuntimeParams["application_name"] = appName
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	p.repository.pool = pool
	service := NewService(p.repository).WithPayments(p)
	server := httptest.NewServer(service.Handler(strings.Repeat("r", 32)))
	defer server.Close()
	p.callbackURL = server.URL + "/internal/payment/notifications"
	provider := &concurrentRefundNotificationProvider{recoverableProvider: &recoverableProvider{}, coordinator: p, appName: appName, delivery: make(chan error, 1)}
	p.provider = provider
	key := uuid.NewString()
	order, _, err := p.CreateOrder(ctx, user, w.ID, "CNY_1", "ALIPAY", key)
	if err != nil {
		t.Fatal(err)
	}
	if order.Status != "MANUAL_REVIEW" || order.FulfillmentStatus != "MANUAL_REVIEW" {
		t.Fatal("create unlocked before persisting the first refunded fact")
	}
	select {
	case err = <-provider.delivery:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("blocked notification did not complete")
	}
	stored, err := p.GetOrder(ctx, order.ID, user)
	wallet, walletErr := p.repository.GetWallet(ctx, w.ID)
	if err != nil || walletErr != nil || stored.Status != "MANUAL_REVIEW" || stored.FulfillmentStatus != "MANUAL_REVIEW" || wallet.PostedMicrocredits != 0 {
		t.Fatal("concurrent late success penetrated the initial refunded review")
	}
	var events, journals, lots int
	if err = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM outbox_events WHERE aggregate_id=$1),(SELECT count(*) FROM journal_entries WHERE business_reference=$2),(SELECT count(*) FROM credit_lots WHERE source_reference=$2)`, order.ID, "recharge:"+order.ID.String()).Scan(&events, &journals, &lots); err != nil || events != 0 || journals != 0 || lots != 0 {
		t.Fatal("concurrent review emitted credit, journal or recharge event")
	}
	replayed, created, err := p.CreateOrder(ctx, user, w.ID, "CNY_1", "ALIPAY", key)
	if err != nil || created || replayed.ID != order.ID || replayed.Status != "MANUAL_REVIEW" || replayed.FulfillmentStatus != "MANUAL_REVIEW" || provider.createCalls != 1 {
		t.Fatal("same-key replay reopened manual review or called the provider again")
	}
}

func TestPostgresRefundedCreateRecoveryClearsOnlyManualReviewReconciliationLease(t *testing.T) {
	ctx := context.Background()
	p, w, user := paymentFixture(t)
	provider := &refundedFirstProvider{recoverableProvider: &recoverableProvider{failFirst: true}, status: "REFUNDED"}
	p.provider = provider
	key := uuid.NewString()
	if _, _, err := p.CreateOrder(ctx, user, w.ID, "CNY_1", "ALIPAY", key); !errors.Is(err, ErrPaymentProvider) {
		t.Fatal("controlled first provider response loss missing")
	}
	if _, err := p.repository.pool.Exec(ctx, `UPDATE payment_orders SET reconciliation_next_at=now()-interval '1 minute',reconciliation_lease_until=now()+interval '1 minute',reconciliation_last_error='controlled provider failure' WHERE requester_id=$1`, user); err != nil {
		t.Fatal(err)
	}
	order, _, err := p.CreateOrder(ctx, user, w.ID, "CNY_1", "ALIPAY", key)
	if err != nil || order.Status != "MANUAL_REVIEW" {
		t.Fatal("refunded create recovery did not enter review")
	}
	var active bool
	if err = p.repository.pool.QueryRow(ctx, `SELECT reconciliation_next_at IS NOT NULL OR reconciliation_lease_until IS NOT NULL OR reconciliation_last_error IS NOT NULL FROM payment_orders WHERE id=$1`, order.ID).Scan(&active); err != nil || active {
		t.Fatal("manual review retained an automatic reconciliation lease")
	}
}

type refundedFirstProvider struct {
	*recoverableProvider
	status          string
	order           ProviderOrder
	emptyPaymentURL bool
}

func (p *refundedFirstProvider) Capabilities() payment.Capabilities {
	caps := p.recoverableProvider.Capabilities()
	caps.CloseSupported = true // This controlled provider implements the explicit Close path.
	return caps
}

func (p *refundedFirstProvider) Create(ctx context.Context, order payment.Order, callback string) (ProviderOrder, error) {
	fact, err := p.recoverableProvider.Create(ctx, order, callback)
	fact.Status = p.status
	if p.emptyPaymentURL {
		fact.PaymentURL = ""
	}
	p.order = fact
	return fact, err
}
func (p *refundedFirstProvider) Get(context.Context, string) (ProviderOrder, error) {
	return p.order, nil
}
func (p *refundedFirstProvider) Close(context.Context, string) (ProviderOrder, error) {
	return p.order, nil
}

func TestPostgresRefundedFirstObservationCoversCreateClosedFailedAndCloseWithMissingPaymentFacts(t *testing.T) {
	for _, entry := range []string{"create", "sync-closed", "sync-failed", "close"} {
		t.Run(entry, func(t *testing.T) {
			ctx := context.Background()
			p, w, user := paymentFixture(t)
			provider := &refundedFirstProvider{recoverableProvider: &recoverableProvider{}, status: "PENDING"}
			p.provider = provider
			if entry == "create" {
				provider.status = "REFUNDED"
				provider.emptyPaymentURL = true
			}
			order, _, err := p.CreateOrder(ctx, user, w.ID, "CNY_1", "ALIPAY", uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			provider.order.Status = "PARTIALLY_REFUNDED" // No transaction or paid_at facts yet.
			switch entry {
			case "sync-closed", "sync-failed":
				status := "CLOSED"
				if entry == "sync-failed" {
					status = "FAILED"
				}
				if _, err = p.repository.pool.Exec(ctx, `UPDATE payment_orders SET status=$2 WHERE id=$1`, order.ID, status); err != nil {
					t.Fatal(err)
				}
				order, err = p.SyncOrder(ctx, order.ID)
			case "close":
				order, err = p.CloseOrder(ctx, order.ID, user)
			}
			if err != nil || order.Status != "MANUAL_REVIEW" || order.FulfillmentStatus != "MANUAL_REVIEW" {
				t.Fatal("first refunded fact with incomplete payment details bypassed manual review")
			}
			wallet, err := p.repository.GetWallet(ctx, w.ID)
			if err != nil || wallet.PostedMicrocredits != 0 {
				t.Fatal("manual review auto credited missing refund facts")
			}
		})
	}
}
