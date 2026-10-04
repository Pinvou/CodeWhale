package billing

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
)

func billingFixture(t *testing.T) (*PostgresRepository, Wallet) {
	t.Helper()
	r := &PostgresRepository{pool: testdb.Open(t, "billing"), now: func() time.Time { return time.Now().UTC() }}
	w, _, err := r.EnsureWallet(context.Background(), "PERSONAL", uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	return r, w
}

func assertBalanced(t *testing.T, r *PostgresRepository) {
	t.Helper()
	var count int
	if err := r.pool.QueryRow(context.Background(), `SELECT count(*) FROM (SELECT journal_entry_id FROM ledger_postings GROUP BY journal_entry_id HAVING sum(delta_microcredits) <> 0) j`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unbalanced journal: %d, %v", count, err)
	}
}

func TestPostgresReserveConcurrentSameUsage(t *testing.T) {
	r, w := billingFixture(t)
	ctx := context.Background()
	if _, err := r.Credit(ctx, w.ID, "initial", 100, true); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	start := make(chan struct{})
	results := make(chan Hold, 8)
	errors := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			h, err := r.Reserve(ctx, id, w.ID, 100, nil, time.Now().Add(time.Hour))
			results <- h
			errors <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	var holdID uuid.UUID
	for h := range results {
		if holdID == uuid.Nil {
			holdID = h.ID
		}
		if h.ID != holdID {
			t.Fatal("same usage produced multiple holds")
		}
	}
	w, err := r.GetWallet(ctx, w.ID)
	if err != nil || w.ReservedMicrocredits != 100 {
		t.Fatalf("wallet: %+v, %v", w, err)
	}
	if _, err := r.Commit(ctx, id, holdID, 60, map[string]any{"input_tokens": 60}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(ctx, id, holdID, 60, map[string]any{"input_tokens": 60}); err != nil {
		t.Fatal(err)
	}
	w, _ = r.GetWallet(ctx, w.ID)
	if w.PostedMicrocredits != 40 || w.ReservedMicrocredits != 0 {
		t.Fatalf("double settlement: %+v", w)
	}
	assertBalanced(t, r)
}

func TestPostgresExpiredHoldsRetainFundsAndConverge(t *testing.T) {
	r, w := billingFixture(t)
	ctx := context.Background()
	if _, err := r.Credit(ctx, w.ID, "initial", 200, true); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	r.now = func() time.Time { return now }
	first, err := r.Reserve(ctx, uuid.New(), w.ID, 100, nil, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Reserve(ctx, uuid.New(), w.ID, 100, nil, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if count, err := r.ExpireHolds(ctx, 100); err != nil || count != 2 {
		t.Fatalf("expire %d, %v", count, err)
	}
	w, _ = r.GetWallet(ctx, w.ID)
	if w.ReservedMicrocredits != 200 || w.AvailableMicrocredits != 0 {
		t.Fatalf("expiry released uncertain funds: %+v", w)
	}
	if h, err := getHoldByUsage(ctx, r.pool, first.UsageRequestID, false); err != nil || h.Status != "REVIEW" {
		t.Fatalf("review %+v %v", h, err)
	}
	var wg sync.WaitGroup
	failures := make(chan error, 3)
	wg.Add(3)
	go func() {
		defer wg.Done()
		_, err := r.Commit(ctx, first.UsageRequestID, first.ID, 70, map[string]any{"input_tokens": 70})
		failures <- err
	}()
	go func() {
		defer wg.Done()
		_, err := r.Release(ctx, second.UsageRequestID, second.ID, "known unused")
		failures <- err
	}()
	go func() { defer wg.Done(); _, err := r.ExpireHolds(ctx, 100); failures <- err }()
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	w, _ = r.GetWallet(ctx, w.ID)
	if w.PostedMicrocredits != 130 || w.ReservedMicrocredits != 0 {
		t.Fatalf("convergence %+v", w)
	}
	assertBalanced(t, r)
}

func TestPostgresExpiryWorkerCancellation(t *testing.T) {
	r, w := billingFixture(t)
	ctx := context.Background()
	if _, err := r.Credit(ctx, w.ID, "initial", 100, true); err != nil {
		t.Fatal(err)
	}
	h, err := r.Reserve(ctx, uuid.New(), w.ID, 100, nil, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE usage_holds SET expires_at=now()-interval '1 second' WHERE id=$1`, h.ID); err != nil {
		t.Fatal(err)
	}
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	workerErrors := make(chan error, 1)
	go func() {
		defer close(done)
		NewService(r).RunHoldExpiry(workerCtx, 5*time.Millisecond, func(err error) {
			select {
			case workerErrors <- err:
			default:
			}
		})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		hold, err := getHoldByUsage(ctx, r.pool, h.UsageRequestID, false)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if hold.Status == "REVIEW" {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("worker did not review overdue hold")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("expiry worker did not stop")
	}
	select {
	case err := <-workerErrors:
		t.Fatal(err)
	default:
	}
	w, _ = r.GetWallet(ctx, w.ID)
	if w.ReservedMicrocredits != 100 {
		t.Fatal("worker released funds")
	}
}

type recoverableProvider struct {
	mu        sync.Mutex
	orders    map[string]ProviderOrder
	failFirst bool
}

func (p *recoverableProvider) Create(_ context.Context, o PaymentOrder, _ string) (ProviderOrder, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.orders == nil {
		p.orders = map[string]ProviderOrder{}
	}
	if result, ok := p.orders[o.MerchantOrderNo]; ok {
		return result, nil
	}
	result := ProviderOrder{MerchantOrderNo: o.MerchantOrderNo, Channel: o.Channel, AmountFen: o.AmountFen, Currency: o.Currency, PaymentURL: "https://payment.example/order/" + o.MerchantOrderNo, Status: "PENDING"}
	p.orders[o.MerchantOrderNo] = result
	if p.failFirst {
		p.failFirst = false
		return ProviderOrder{}, errors.New("response lost after external creation")
	}
	return result, nil
}
func (*recoverableProvider) Get(context.Context, string) (ProviderOrder, error) {
	return ProviderOrder{}, errors.New("unused")
}
func (*recoverableProvider) Close(context.Context, string) (ProviderOrder, error) {
	return ProviderOrder{}, errors.New("unused")
}
func (*recoverableProvider) Refund(context.Context, string, string, int64) (ProviderRefund, error) {
	return ProviderRefund{}, errors.New("unused")
}
func (*recoverableProvider) Bill(context.Context, string, time.Time) ([]ProviderBillEntry, error) {
	return nil, errors.New("unused")
}

func paymentFixture(t *testing.T) (*PaymentCoordinator, Wallet, uuid.UUID) {
	t.Helper()
	r, w := billingFixture(t)
	p := &PaymentCoordinator{repository: r, provider: &recoverableProvider{}, now: r.now, merchantID: "pinvou-development", callbackURL: "https://billing.example/callback", secrets: map[string][]byte{"ALIPAY": []byte("test-only-notification-secret-32-chars")}}
	return p, w, uuid.New()
}

func TestPostgresPaymentCreateRecoversExternalOutcomeAndConcurrentReplay(t *testing.T) {
	p, w, user := paymentFixture(t)
	ctx := context.Background()
	provider := &recoverableProvider{failFirst: true}
	p.provider = provider
	var product string
	if err := p.repository.pool.QueryRow(ctx, `SELECT product_id FROM recharge_products LIMIT 1`).Scan(&product); err != nil {
		t.Fatal(err)
	}
	key := "payment-recovery-key-001"
	if _, _, err := p.CreateOrder(ctx, user, w.ID, product, "ALIPAY", key); !errors.Is(err, ErrPaymentProvider) {
		t.Fatalf("missing simulated response-loss: %v", err)
	}
	var prepared PaymentOrder
	var body []byte
	if err := p.repository.pool.QueryRow(ctx, `SELECT response_body FROM idempotency_records WHERE idempotency_key=$1`, key).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &prepared); err != nil || prepared.Status != "CREATED" {
		t.Fatalf("durable prepare %s %v", body, err)
	}
	var wg sync.WaitGroup
	failures := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			order, created, err := p.CreateOrder(ctx, user, w.ID, product, "ALIPAY", key)
			if err == nil && (created || order.ID != prepared.ID || order.MerchantOrderNo != prepared.MerchantOrderNo || order.PaymentURL == "" || order.Status != "PENDING") {
				err = errors.New("replay changed payment identity")
			}
			failures <- err
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(provider.orders) != 1 {
		t.Fatalf("external orders: %d", len(provider.orders))
	}
	if _, _, err := p.CreateOrder(ctx, uuid.New(), w.ID, product, "ALIPAY", key); !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-user replay %v", err)
	}
}

func creditedOrder(t *testing.T, p *PaymentCoordinator, w Wallet, user uuid.UUID, key string) PaymentOrder {
	t.Helper()
	ctx := context.Background()
	var product string
	if err := p.repository.pool.QueryRow(ctx, `SELECT product_id FROM recharge_products ORDER BY amount_fen LIMIT 1`).Scan(&product); err != nil {
		t.Fatal(err)
	}
	o, _, err := p.CreateOrder(ctx, user, w.ID, product, "ALIPAY", key)
	if err != nil {
		t.Fatal(err)
	}
	paidAt := p.now()
	txn := "transaction-" + o.ID.String()
	o, err = p.applyQueriedPayment(ctx, o.ID, ProviderOrder{MerchantOrderNo: o.MerchantOrderNo, Channel: o.Channel, AmountFen: o.AmountFen, Currency: o.Currency, Status: "SUCCESS", PaidAt: &paidAt, ChannelTransactionID: &txn})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestPostgresPendingRefundProtectsOriginalLot(t *testing.T) {
	for _, debit := range []string{"usage", "adjustment"} {
		t.Run(debit, func(t *testing.T) {
			p, w, user := paymentFixture(t)
			ctx := context.Background()
			a := creditedOrder(t, p, w, user, "recharge-order-first")
			b := creditedOrder(t, p, w, user, "recharge-order-second")
			refund, _, err := p.reserveRefund(ctx, a.ID, user, a.AmountFen, "refund-original-lot", "hash-original", "full unused lot")
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := p.reserveRefund(ctx, a.ID, user, a.AmountFen, "refund-duplicate-lot", "another-hash", "second full refund"); !errors.Is(err, ErrInsufficientBalance) {
				t.Fatalf("oversubscribed source lot: %v", err)
			}
			if debit == "usage" {
				h, err := p.repository.Reserve(ctx, uuid.New(), w.ID, b.CreditMicrocredits, nil, time.Now().Add(time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := p.repository.Commit(ctx, h.UsageRequestID, h.ID, b.CreditMicrocredits, map[string]any{"input_tokens": 1}); err != nil {
					t.Fatal(err)
				}
			} else {
				adjustment, err := p.repository.RequestAdjustment(ctx, w.ID, -b.CreditMicrocredits, "correct other balance", uuid.New())
				if err != nil {
					t.Fatal(err)
				}
				if _, err := p.repository.ApproveAdjustment(ctx, adjustment.ID, uuid.New(), "independent review"); err != nil {
					t.Fatal(err)
				}
			}
			var remaining int64
			if err := p.repository.pool.QueryRow(ctx, `SELECT remaining_microcredits FROM credit_lots WHERE source_reference=$1`, "recharge:"+a.ID.String()).Scan(&remaining); err != nil || remaining != a.CreditMicrocredits {
				t.Fatalf("refund lot consumed: %d %v", remaining, err)
			}
			result, _, err := p.finishRefund(ctx, refund, ProviderRefund{MerchantRefundNo: refund.MerchantRefundNo, ChannelRefundID: "refund-channel-id", AmountFen: refund.AmountFen, Status: "SUCCESS"}, true)
			if err != nil || result.Status != "SUCCEEDED" {
				t.Fatalf("refund %+v %v", result, err)
			}
			wallet, err := p.repository.GetWallet(ctx, w.ID)
			if err != nil || wallet.PostedMicrocredits != 0 || wallet.ReservedMicrocredits != 0 {
				t.Fatalf("wallet %+v %v", wallet, err)
			}
			assertBalanced(t, p.repository)
		})
	}
}

func TestPostgresRechargeNotificationAfterFullRefundIsIdempotent(t *testing.T) {
	p, w, user := paymentFixture(t)
	ctx := context.Background()
	o := creditedOrder(t, p, w, user, "recharge-callback-test")
	ref, _, err := p.reserveRefund(ctx, o.ID, user, o.AmountFen, "refund-callback-test", "hash", "unused refund")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.finishRefund(ctx, ref, ProviderRefund{MerchantRefundNo: ref.MerchantRefundNo, AmountFen: ref.AmountFen, Status: "SUCCESS", ChannelRefundID: "refund-channel"}, true); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		body, _ := json.Marshal(map[string]any{"notification_id": uuid.New(), "merchant_id": p.merchantID, "merchant_order_no": o.MerchantOrderNo, "channel_transaction_id": *o.ChannelTransactionID, "channel": o.Channel, "status": "SUCCESS", "amount_fen": o.AmountFen, "currency": o.Currency, "paid_at": o.PaidAt, "timestamp": p.now().Unix()})
		if err := p.ProcessNotification(ctx, o.Channel, paymentSignature(p.secrets[o.Channel], body), body); err != nil {
			t.Fatal(err)
		}
	}
	var events int
	if err := p.repository.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='billing.recharge.succeeded.v1'`, o.ID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("events %d %v", events, err)
	}
	wallet, err := p.repository.GetWallet(ctx, w.ID)
	if err != nil || wallet.PostedMicrocredits != 0 {
		t.Fatalf("recredited refunded order %+v %v", wallet, err)
	}
	assertBalanced(t, p.repository)
}

func TestPostgresNotificationRetryOnlyTimestampMayChange(t *testing.T) {
	p, w, user := paymentFixture(t)
	ctx := context.Background()
	o := creditedOrder(t, p, w, user, "recharge-retry-window")
	now := p.now()
	p.now = func() time.Time { return now }
	id := uuid.New()
	body, _ := json.Marshal(map[string]any{"notification_id": id, "merchant_id": p.merchantID, "merchant_order_no": o.MerchantOrderNo, "channel_transaction_id": *o.ChannelTransactionID, "channel": o.Channel, "status": "SUCCESS", "amount_fen": o.AmountFen, "currency": o.Currency, "paid_at": o.PaidAt, "timestamp": now.Unix()})
	if err := p.ProcessNotification(ctx, o.Channel, paymentSignature(p.secrets[o.Channel], body), body); err != nil {
		t.Fatal(err)
	}
	var originalHash string
	if err := p.repository.pool.QueryRow(ctx, `SELECT request_hash FROM payment_notifications WHERE notification_id=$1`, id).Scan(&originalHash); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Minute)
	if err := p.ProcessNotification(ctx, o.Channel, paymentSignature(p.secrets[o.Channel], body), body); !errors.Is(err, ErrForbiddenPayment) {
		t.Fatalf("old signature window accepted: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	fields["timestamp"], _ = json.Marshal(now.Unix())
	retry, _ := json.Marshal(fields)
	if err := p.ProcessNotification(ctx, o.Channel, paymentSignature(p.secrets[o.Channel], retry), retry); err != nil {
		t.Fatalf("fresh retry rejected: %v", err)
	}
	var afterHash string
	if err := p.repository.pool.QueryRow(ctx, `SELECT request_hash FROM payment_notifications WHERE notification_id=$1`, id).Scan(&afterHash); err != nil || afterHash != originalHash {
		t.Fatal("retry rewrote original audit hash")
	}
	changes := map[string]json.RawMessage{"amount_fen": json.RawMessage("9007199254740993"), "currency": json.RawMessage(`"USD"`), "merchant_id": json.RawMessage(`"different-merchant"`), "merchant_order_no": json.RawMessage(`"different-order"`), "channel_transaction_id": json.RawMessage(`"different-transaction"`), "channel": json.RawMessage(`"WECHAT"`), "status": json.RawMessage(`"FAILED"`), "paid_at": json.RawMessage(`"2000-01-01T00:00:00Z"`), "unknown_delivery_field": json.RawMessage(`"changed"`)}
	for field, value := range changes {
		t.Run(field, func(t *testing.T) {
			var changed map[string]json.RawMessage
			json.Unmarshal(retry, &changed)
			changed[field] = value
			payload, _ := json.Marshal(changed)
			if err := p.ProcessNotification(ctx, o.Channel, paymentSignature(p.secrets[o.Channel], payload), payload); !errors.Is(err, ErrForbiddenPayment) {
				t.Fatalf("changed fact accepted: %v", err)
			}
		})
	}
}

type closeReplyProvider struct {
	*recoverableProvider
	order  ProviderOrder
	closed ProviderOrder
}

func (p *closeReplyProvider) Get(context.Context, string) (ProviderOrder, error) { return p.order, nil }
func (p *closeReplyProvider) Close(context.Context, string) (ProviderOrder, error) {
	return p.closed, nil
}

func TestPostgresMismatchedCloseResponseCannotCloseOrder(t *testing.T) {
	for _, mode := range []string{"direct", "expired-sync"} {
		t.Run(mode, func(t *testing.T) {
			p, w, user := paymentFixture(t)
			ctx := context.Background()
			o, _, err := p.CreateOrder(ctx, user, w.ID, "CNY_1", "ALIPAY", "close-mismatch-test")
			if err != nil {
				t.Fatal(err)
			}
			provider := &closeReplyProvider{order: ProviderOrder{MerchantOrderNo: o.MerchantOrderNo, Channel: o.Channel, AmountFen: o.AmountFen, Currency: o.Currency, Status: "PENDING"}, closed: ProviderOrder{MerchantOrderNo: "another-order", Channel: o.Channel, AmountFen: o.AmountFen, Currency: o.Currency, Status: "CLOSED"}}
			p.provider = provider
			if mode == "direct" {
				_, err = p.CloseOrder(ctx, o.ID, user)
			} else {
				p.now = func() time.Time { return o.ExpiresAt.Add(time.Second) }
				_, err = p.SyncOrder(ctx, o.ID)
			}
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("mismatched close accepted: %v", err)
			}
			stored, err := p.GetOrder(ctx, o.ID, user)
			if err != nil || stored.Status != "PENDING" {
				t.Fatalf("local order changed %+v %v", stored, err)
			}
		})
	}
}

func TestPostgresWalletGuardRollbackLeavesHoldAndJournalUntouched(t *testing.T) {
	for _, operation := range []string{"commit", "release"} {
		t.Run(operation, func(t *testing.T) {
			r, w := billingFixture(t)
			ctx := context.Background()
			if _, err := r.Credit(ctx, w.ID, "initial", 100, true); err != nil {
				t.Fatal(err)
			}
			h, err := r.Reserve(ctx, uuid.New(), w.ID, 100, nil, time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			// Deliberately inject inconsistent state in the isolated fixture to prove
			// conditional wallet updates cannot silently finalize a hold.
			if _, err := r.pool.Exec(ctx, `UPDATE wallets SET reserved_microcredits=0 WHERE id=$1`, w.ID); err != nil {
				t.Fatal(err)
			}
			if operation == "commit" {
				_, err = r.Commit(ctx, h.UsageRequestID, h.ID, 50, map[string]any{"input_tokens": 50})
			} else {
				_, err = r.Release(ctx, h.UsageRequestID, h.ID, "known unused")
			}
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("wallet guard ignored: %v", err)
			}
			hold, err := getHoldByUsage(ctx, r.pool, h.UsageRequestID, false)
			if err != nil || hold.Status != "ACTIVE" {
				t.Fatalf("hold finalized %+v %v", hold, err)
			}
			var count int
			if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE business_reference=$1`, "usage:"+h.UsageRequestID.String()).Scan(&count); err != nil || count != 0 {
				t.Fatalf("partial journal %d %v", count, err)
			}
		})
	}
}

func TestPostgresFailedFulfillmentRetainsReceiptAndRetries(t *testing.T) {
	p, w, user := paymentFixture(t)
	ctx := context.Background()
	o, _, err := p.CreateOrder(ctx, user, w.ID, "CNY_1", "ALIPAY", "receipt-retry-wallet-key")
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	paidAt := p.now()
	body, _ := json.Marshal(map[string]any{"notification_id": id, "merchant_id": p.merchantID, "merchant_order_no": o.MerchantOrderNo, "channel_transaction_id": "transaction-receipt-retry", "channel": o.Channel, "status": "SUCCESS", "amount_fen": o.AmountFen, "currency": o.Currency, "paid_at": paidAt, "timestamp": p.now().Unix()})
	if _, err := p.repository.pool.Exec(ctx, `UPDATE wallets SET status='FROZEN' WHERE id=$1`, w.ID); err != nil {
		t.Fatal(err)
	}
	if err := p.ProcessNotification(ctx, o.Channel, paymentSignature(p.secrets[o.Channel], body), body); !errors.Is(err, ErrWalletUnavailable) {
		t.Fatalf("frozen fulfillment %v", err)
	}
	var result, hash string
	if err := p.repository.pool.QueryRow(ctx, `SELECT result,request_hash FROM payment_notifications WHERE notification_id=$1`, id).Scan(&result, &hash); err != nil || result != "RECEIVED" {
		t.Fatalf("lost verified receipt %s %v", result, err)
	}
	var entries int
	if err := p.repository.pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE business_reference=$1`, "recharge:"+o.ID.String()).Scan(&entries); err != nil || entries != 0 {
		t.Fatal("failed fulfillment partially credited")
	}
	if _, err := p.repository.pool.Exec(ctx, `UPDATE wallets SET status='ACTIVE' WHERE id=$1`, w.ID); err != nil {
		t.Fatal(err)
	}
	if err := p.ProcessNotification(ctx, o.Channel, paymentSignature(p.secrets[o.Channel], body), body); err != nil {
		t.Fatal(err)
	}
	var afterHash string
	if err := p.repository.pool.QueryRow(ctx, `SELECT result,request_hash FROM payment_notifications WHERE notification_id=$1`, id).Scan(&result, &afterHash); err != nil || result != "PROCESSED" || hash != afterHash {
		t.Fatalf("retry lost audit %s %v", result, err)
	}
	wallet, err := p.repository.GetWallet(ctx, w.ID)
	if err != nil || wallet.PostedMicrocredits != o.CreditMicrocredits {
		t.Fatalf("retry failed %+v %v", wallet, err)
	}
	assertBalanced(t, p.repository)
}

func TestPostgresConcurrentCallbacksCreditAndEmitOnlyOnce(t *testing.T) {
	for _, mode := range []string{"same-notification", "distinct-notifications"} {
		t.Run(mode, func(t *testing.T) {
			p, w, user := paymentFixture(t)
			ctx := context.Background()
			o, _, err := p.CreateOrder(ctx, user, w.ID, "CNY_1", "ALIPAY", "concurrent-callback-key")
			if err != nil {
				t.Fatal(err)
			}
			paidAt, id := p.now(), uuid.New()
			start := make(chan struct{})
			failures := make(chan error, 8)
			var wg sync.WaitGroup
			for range 8 {
				currentID := id
				if mode == "distinct-notifications" {
					currentID = uuid.New()
				}
				body, _ := json.Marshal(map[string]any{"notification_id": currentID, "merchant_id": p.merchantID, "merchant_order_no": o.MerchantOrderNo, "channel_transaction_id": "transaction-concurrent", "channel": o.Channel, "status": "SUCCESS", "amount_fen": o.AmountFen, "currency": o.Currency, "paid_at": paidAt, "timestamp": p.now().Unix()})
				wg.Add(1)
				go func(payload []byte) {
					defer wg.Done()
					<-start
					failures <- p.ProcessNotification(ctx, o.Channel, paymentSignature(p.secrets[o.Channel], payload), payload)
				}(body)
			}
			close(start)
			wg.Wait()
			close(failures)
			for err := range failures {
				if err != nil {
					t.Fatal(err)
				}
			}
			var entries, events int
			if err := p.repository.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM journal_entries WHERE business_reference=$1),(SELECT count(*) FROM outbox_events WHERE aggregate_id=$2)`, "recharge:"+o.ID.String(), o.ID).Scan(&entries, &events); err != nil || entries != 1 || events != 1 {
				t.Fatalf("duplicate effects %d %d %v", entries, events, err)
			}
			wallet, err := p.repository.GetWallet(ctx, w.ID)
			if err != nil || wallet.PostedMicrocredits != o.CreditMicrocredits {
				t.Fatalf("double balance %+v %v", wallet, err)
			}
			assertBalanced(t, p.repository)
		})
	}
}

func TestPostgresNotificationRouteChannelCannotReplayOrPersistUnknownChannel(t *testing.T) {
	p, w, user := paymentFixture(t)
	ctx := context.Background()
	o := creditedOrder(t, p, w, user, "channel-notification-key")
	p.secrets["WECHAT"] = p.secrets["ALIPAY"]
	body, _ := json.Marshal(map[string]any{"notification_id": uuid.New(), "merchant_id": p.merchantID, "merchant_order_no": o.MerchantOrderNo, "channel_transaction_id": *o.ChannelTransactionID, "channel": o.Channel, "status": "SUCCESS", "amount_fen": o.AmountFen, "currency": o.Currency, "paid_at": o.PaidAt, "timestamp": p.now().Unix()})
	if err := p.ProcessNotification(ctx, o.Channel, paymentSignature(p.secrets[o.Channel], body), body); err != nil {
		t.Fatal(err)
	}
	if err := p.ProcessNotification(ctx, "WECHAT", paymentSignature(p.secrets["WECHAT"], body), body); !errors.Is(err, ErrForbiddenPayment) {
		t.Fatalf("cross-channel replay accepted: %v", err)
	}
	var payload map[string]json.RawMessage
	json.Unmarshal(body, &payload)
	unknownID := uuid.New()
	payload["notification_id"], _ = json.Marshal(unknownID)
	unknownBody, _ := json.Marshal(payload)
	if err := p.ProcessNotification(ctx, "UNKNOWN", paymentSignature(nil, unknownBody), unknownBody); !errors.Is(err, ErrForbiddenPayment) {
		t.Fatalf("unknown channel accepted: %v", err)
	}
	var count int
	if err := p.repository.pool.QueryRow(ctx, `SELECT count(*) FROM payment_notifications WHERE notification_id=$1`, unknownID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unknown channel persisted receipt: %d %v", count, err)
	}
}

func TestPostgresUnavailableWalletCannotCreateProviderPayment(t *testing.T) {
	for _, mode := range []string{"new", "prepared-recovery"} {
		t.Run(mode, func(t *testing.T) {
			p, w, user := paymentFixture(t)
			ctx := context.Background()
			provider := &recoverableProvider{}
			p.provider = provider
			var prepared PaymentOrder
			if mode == "prepared-recovery" {
				provider.failFirst = true
				var err error
				prepared, _, err = p.CreateOrder(ctx, user, w.ID, "CNY_1", "ALIPAY", "frozen-wallet-payment")
				if !errors.Is(err, ErrPaymentProvider) {
					t.Fatal(err)
				}
			}
			if _, err := p.repository.pool.Exec(ctx, `UPDATE wallets SET status='FROZEN' WHERE id=$1`, w.ID); err != nil {
				t.Fatal(err)
			}
			_, _, err := p.CreateOrder(ctx, user, w.ID, "CNY_1", "ALIPAY", "frozen-wallet-payment")
			if !errors.Is(err, ErrWalletUnavailable) {
				t.Fatalf("frozen wallet accepted %v", err)
			}
			if mode == "new" {
				if len(provider.orders) != 0 {
					t.Fatal("created external order for frozen wallet")
				}
			} else {
				if len(provider.orders) != 1 {
					t.Fatal("duplicated unresolved provider order")
				}
				stored, err := p.GetOrder(ctx, prepared.ID, user)
				if err != nil || stored.Status != "CREATED" {
					t.Fatalf("lost prepared order %+v %v", stored, err)
				}
			}
		})
	}
}
