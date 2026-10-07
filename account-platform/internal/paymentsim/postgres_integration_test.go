package paymentsim

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/paymentsimulator"
	"github.com/Pinvou/pinvou3/account-platform/internal/simulatorschema"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Only a randomly named owned schema is created/dropped. No public/global roles or existing data are changed.
func simulatorDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	raw := os.Getenv("PINVOU_TEST_PAYMENT_SIM_DATABASE_URL")
	if raw == "" {
		raw = os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL")
	}
	if raw == "" {
		t.Skip("owned PostgreSQL fixture URL is not configured")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal("owned simulator database unavailable")
	}
	schema := "sim_f02_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		admin.Close(ctx)
		t.Fatal("owned simulator schema creation failed")
	}
	var pool *pgxpool.Pool
	t.Cleanup(func() {
		if pool != nil {
			pool.Close()
		}
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error("owned simulator schema cleanup failed")
		}
		var exists bool
		if err := admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)`, schema).Scan(&exists); err != nil || exists {
			t.Error("owned simulator schema remains")
		}
		admin.Close(ctx)
	})
	cfg, err := pgx.ParseConfig(raw)
	if err != nil {
		t.Fatal("owned simulator connection configuration invalid")
	}
	cfg.RuntimeParams["search_path"] = schema
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal("owned schema connection failed")
	}
	list, err := migrations.Load(filepath.Join("..", "..", "migrations", "payment-simulator"))
	if err != nil {
		conn.Close(ctx)
		t.Fatal("simulator SQL load failed")
	}
	if err = simulatorschema.Up(ctx, conn, list); err != nil {
		conn.Close(ctx)
		t.Fatal("simulator SQL upgrade failed")
	}
	conn.Close(ctx)
	pcfg, err := pgxpool.ParseConfig(raw)
	if err != nil {
		t.Fatal("owned pool configuration invalid")
	}
	pcfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err = pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		t.Fatal("owned pool creation failed")
	}
	return pool
}

func TestPostgresSimulatorMappedFactsOverAuthenticatedHTTP(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	r, _ := simulatorFixture(t, "SUCCESS")
	const token = "controlled-simulator-service-token"
	server := httptest.NewServer(NewService(r).Handler(token))
	defer server.Close()
	provider, err := paymentsimulator.New(server.URL, token)
	if err != nil {
		t.Fatal("provider initialization failed")
	}
	ctx := context.Background()
	request := payment.Order{MerchantOrderNo: uuid.NewString(), MerchantCustomerKey: uuid.NewString(), Mobile: "+8613800138000", ExternalSKU: "controlled-product", Channel: "ALIPAY", AmountFen: 100, Currency: "CNY"}
	callback := "http://127.0.0.1/controlled-unused"
	created, err := provider.Create(ctx, request, callback)
	if err != nil || created.ExternalCustomerKey != request.MerchantCustomerKey || created.ExternalSKU != request.ExternalSKU || created.AmountFen != request.AmountFen {
		t.Fatal("creation did not return real requested mapping facts")
	}
	stored, err := r.GetByMerchantOrder(ctx, request.MerchantOrderNo)
	if err != nil || stored.Mobile != request.Mobile || stored.MerchantCustomerKey != request.MerchantCustomerKey || stored.ProviderSKU != request.ExternalSKU {
		t.Fatal("mapping facts were not durably stored")
	}
	replay, err := provider.Create(ctx, request, callback)
	if err != nil || replay.ExternalCustomerKey != created.ExternalCustomerKey || replay.ExternalSKU != created.ExternalSKU || replay.PaymentURL != created.PaymentURL {
		t.Fatal("same-key replay changed stored facts")
	}
	for _, field := range []string{"customer", "mobile", "sku", "amount", "callback"} {
		changed := request
		changedCallback := callback
		switch field {
		case "customer":
			changed.MerchantCustomerKey = uuid.NewString()
		case "mobile":
			changed.Mobile = "+8613900139000"
		case "sku":
			changed.ExternalSKU = "changed"
		case "amount":
			changed.AmountFen++
		case "callback":
			changedCallback += "-changed"
		}
		if _, err := provider.Create(ctx, changed, changedCallback); err == nil {
			t.Fatal("conflicting immutable creation facts accepted")
		}
	}
	got, err := provider.Get(ctx, request.MerchantOrderNo)
	if err != nil || got.ExternalCustomerKey != created.ExternalCustomerKey || got.ExternalSKU != created.ExternalSKU {
		t.Fatal("query lost durable mapping")
	}
	response, err := http.Get(server.URL + "/internal/v1/orders/" + request.MerchantOrderNo)
	if err != nil {
		t.Fatal("unauthenticated request failed")
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatal("private mapping response exposed without authentication")
	}
	response, err = http.Get(server.URL + "/pay/" + r.paymentToken(request.MerchantOrderNo))
	if err != nil {
		t.Fatal("public payment page failed")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || strings.Contains(string(body), request.Mobile) || strings.Contains(string(body), request.ExternalSKU) || strings.Contains(string(body), request.MerchantCustomerKey) {
		t.Fatal("private customer mapping leaked into public payment page")
	}
}

func TestPostgresSimulatorDelayedNotificationRetainsPaymentFacts(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	now := time.Now().UTC()
	r, _ := simulatorFixture(t, "SUCCESS")
	r.now = func() time.Time { return now }
	customer := uuid.NewString()
	const sku = "controlled-stable-notification-sku"
	var initial map[string]json.RawMessage
	var deliveries int
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
		if err != nil {
			t.Error("callback body read failed")
			w.WriteHeader(400)
			return
		}
		if !hmac.Equal([]byte(sign(r.hmacSecrets["ALIPAY"], body)), []byte(request.Header.Get("X-Pinvou-Payment-Signature"))) {
			t.Error("callback bytes were not signed")
			w.WriteHeader(400)
			return
		}
		var facts map[string]json.RawMessage
		if json.Unmarshal(body, &facts) != nil {
			t.Error("callback JSON invalid")
			w.WriteHeader(400)
			return
		}
		var timestamp int64
		if json.Unmarshal(facts["timestamp"], &timestamp) != nil || timestamp != now.Unix() {
			t.Error("callback timestamp was not refreshed")
			w.WriteHeader(400)
			return
		}
		delete(facts, "timestamp")
		if deliveries == 0 {
			initial = facts
		} else {
			before, _ := json.Marshal(initial)
			after, _ := json.Marshal(facts)
			if string(before) != string(after) {
				t.Error("notification ID or payment facts changed on retry")
			}
		}
		deliveries++
		if deliveries == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer peer.Close()
	order, _, err := r.Create(context.Background(), CreateOrder{MerchantOrderNo: uuid.NewString(), MerchantCustomerKey: customer, ProviderSKU: sku, Channel: "ALIPAY", AmountFen: 9007199254740993, Currency: "CNY", CallbackURL: peer.URL, Scenario: "SUCCESS", ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal("notification fixture failed")
	}
	if _, err := r.Complete(context.Background(), r.paymentToken(order.MerchantOrderNo)); err != nil {
		t.Fatal("notification payment failed")
	}
	service := NewService(r)
	if err := service.DeliverPending(context.Background()); err != nil || deliveries != 1 {
		t.Fatal("first callback attempt did not execute")
	}
	now = now.Add(6 * time.Minute)
	if err := service.DeliverPending(context.Background()); err != nil || deliveries != 2 {
		t.Fatal("delayed callback retry did not execute")
	}
	var status string
	if err := r.pool.QueryRow(context.Background(), `SELECT status FROM simulator_notifications WHERE order_id=$1`, order.ID).Scan(&status); err != nil || status != "DELIVERED" {
		t.Fatal("confirmed callback did not reach durable delivered state")
	}
	var amount int64
	if json.Unmarshal(initial["amount_fen"], &amount) != nil || amount != order.AmountFen || len(initial) != 11 {
		t.Fatal("strict original payment payload was altered")
	}
	var actualCustomer, actualSKU string
	if json.Unmarshal(initial["external_customer_key"], &actualCustomer) != nil || json.Unmarshal(initial["external_sku"], &actualSKU) != nil || actualCustomer != customer || actualSKU != sku {
		t.Fatal("notification lost durably frozen customer or SKU")
	}
}
func simulatorFixture(t *testing.T, scenario string) (*PostgresRepository, Order) {
	t.Helper()
	r := &PostgresRepository{pool: simulatorDatabase(t), publicURL: "https://simulator.example", tokenSecret: []byte("test-only-token-secret-thirty-two-chars"), hmacSecrets: map[string][]byte{"ALIPAY": []byte("test-only-hmac-secret-thirty-two-chars")}, now: func() time.Time { return time.Now().UTC() }}
	o, _, err := r.Create(context.Background(), CreateOrder{MerchantOrderNo: uuid.NewString(), Channel: "ALIPAY", AmountFen: 100, Currency: "CNY", CallbackURL: "https://billing.example/notify", Scenario: scenario, ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	return r, o
}

func TestPostgresSimulatorConcurrentRefundIdempotency(t *testing.T) {
	r, o := simulatorFixture(t, "SUCCESS")
	ctx := context.Background()
	if _, err := r.Complete(ctx, r.paymentToken(o.MerchantOrderNo)); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	failures := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			refund, _, err := r.Refund(ctx, o.MerchantOrderNo, "stable-refund-merchant", 100)
			if err == nil && refund.Status != "SUCCESS" {
				err = errors.New("refund not successful")
			}
			failures <- err
		}()
	}
	close(start)
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	result, err := r.GetByMerchantOrder(ctx, o.MerchantOrderNo)
	if err != nil || result.RefundedAmountFen != 100 || result.Status != "REFUNDED" {
		t.Fatalf("refund duplicated %+v %v", result, err)
	}
	if _, _, err := r.Refund(ctx, "another-order", "stable-refund-merchant", 100); !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-order idempotency %v", err)
	}
}

func TestPostgresSimulatorNotificationLeaseFencesStaleDelivery(t *testing.T) {
	r, o := simulatorFixture(t, "SUCCESS")
	ctx := context.Background()
	if _, err := r.Complete(ctx, r.paymentToken(o.MerchantOrderNo)); err != nil {
		t.Fatal(err)
	}
	first, err := r.ClaimNotifications(ctx, 1)
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim %+v %v", first, err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE simulator_notifications SET next_attempt_at=now()-interval '1 second' WHERE id=$1`, first[0].ID); err != nil {
		t.Fatal(err)
	}
	second, err := r.ClaimNotifications(ctx, 1)
	if err != nil || len(second) != 1 || second[0].Attempts != first[0].Attempts+1 {
		t.Fatalf("second claim %+v %v", second, err)
	}
	if err := r.MarkNotification(ctx, second[0], nil); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkNotification(ctx, first[0], errors.New("late failure")); err != nil {
		t.Fatal(err)
	}
	var status string
	var attempts int
	if err := r.pool.QueryRow(ctx, `SELECT status,attempts FROM simulator_notifications WHERE id=$1`, first[0].ID).Scan(&status, &attempts); err != nil || status != "DELIVERED" || attempts != 2 {
		t.Fatalf("stale failure resurrected notification %s %d %v", status, attempts, err)
	}
}

func TestPostgresSimulatorDuplicateNotificationAndRefundFailure(t *testing.T) {
	for _, scenario := range []string{"DUPLICATE_NOTIFY", "REFUND_FAIL"} {
		t.Run(scenario, func(t *testing.T) {
			r, o := simulatorFixture(t, scenario)
			ctx := context.Background()
			if _, err := r.Complete(ctx, r.paymentToken(o.MerchantOrderNo)); err != nil {
				t.Fatal(err)
			}
			if scenario == "REFUND_FAIL" {
				f, _, err := r.Refund(ctx, o.MerchantOrderNo, "failed-refund", 50)
				if err != nil || f.Status != "FAILED" {
					t.Fatalf("refund %+v %v", f, err)
				}
				got, _ := r.GetByMerchantOrder(ctx, o.MerchantOrderNo)
				if got.RefundedAmountFen != 0 {
					t.Fatal("failed refund moved funds")
				}
				return
			}
			items, err := r.ClaimNotifications(ctx, 1)
			if err != nil || len(items) != 1 || items[0].RemainingDeliveries != 2 {
				t.Fatalf("duplicate scenario %+v %v", items, err)
			}
			if err := r.MarkNotification(ctx, items[0], nil); err != nil {
				t.Fatal(err)
			}
			if _, err := r.pool.Exec(ctx, `UPDATE simulator_notifications SET next_attempt_at=now()-interval '1 second' WHERE id=$1`, items[0].ID); err != nil {
				t.Fatal(err)
			}
			second, err := r.ClaimNotifications(ctx, 1)
			if err != nil || len(second) != 1 || second[0].RemainingDeliveries != 1 {
				t.Fatalf("second delivery %+v %v", second, err)
			}
			if err := r.MarkNotification(ctx, second[0], nil); err != nil {
				t.Fatal(err)
			}
			remaining, err := r.ClaimNotifications(ctx, 1)
			if err != nil || len(remaining) != 0 {
				t.Fatalf("completed duplicate notification still claimed %+v %v", remaining, err)
			}
		})
	}
}

func TestPostgresSimulatorPreservesBadSignatureScenario(t *testing.T) {
	r, o := simulatorFixture(t, "BAD_SIGNATURE")
	ctx := context.Background()
	if _, err := r.Complete(ctx, r.paymentToken(o.MerchantOrderNo)); err != nil {
		t.Fatal(err)
	}
	items, err := r.ClaimNotifications(ctx, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("bad-signature claim %+v %v", items, err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		mac := hmac.New(sha256.New, r.hmacSecrets["ALIPAY"])
		mac.Write(body)
		expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
		if hmac.Equal([]byte(expected), []byte(request.Header.Get("X-Pinvou-Payment-Signature"))) {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	request, _ := http.NewRequest(http.MethodPost, server.URL, strings.NewReader(string(items[0].Payload)))
	request.Header.Set("X-Pinvou-Payment-Signature", items[0].Signature)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatal("fresh signing removed configured BAD_SIGNATURE failure")
	}
}

func TestPostgresPaymentPageDisplaysLargeFenExactly(t *testing.T) {
	r, o := simulatorFixture(t, "SUCCESS")
	ctx := context.Background()
	const amount int64 = 9007199254740993
	if _, err := r.pool.Exec(ctx, `UPDATE simulator_orders SET amount_fen=$2 WHERE id=$1`, o.ID, amount); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewService(r).Handler(strings.Repeat("t", 32)))
	defer server.Close()
	response, err := server.Client().Get(server.URL + "/pay/" + r.paymentToken(o.MerchantOrderNo))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "90071992547409.93") {
		t.Fatalf("inexact fen display %d %s", response.StatusCode, body)
	}
}

// These tests use actual migrated PostgreSQL and the production authenticated
// Simulator HTTP endpoint/neutral provider adapter and durable Simulator facts;
// this fixture does not execute Billing ledger postings.
func TestPostgresSimulatorBillShanghaiDayBoundaries(t *testing.T) {
	day := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	start := day.Add(-8 * time.Hour)
	for _, tc := range []struct {
		name    string
		instant time.Time
		count   int
	}{
		{"before", start.Add(-time.Microsecond), 0},
		{"at_start", start, 1},
		{"UTC_midnight", day, 1},
		{"before_end", start.Add(24*time.Hour - time.Microsecond), 1},
		{"at_end", start.Add(24 * time.Hour), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r := &PostgresRepository{pool: simulatorDatabase(t), publicURL: "https://controlled-simulator.invalid", tokenSecret: []byte(strings.Repeat("t", 32)), hmacSecrets: map[string][]byte{"ALIPAY": []byte(strings.Repeat("s", 32))}, now: func() time.Time { return tc.instant }}
			o, _, err := r.Create(ctx, CreateOrder{MerchantOrderNo: uuid.NewString(), Channel: "ALIPAY", AmountFen: 100, Currency: "CNY", CallbackURL: "http://127.0.0.1/controlled-unused", Scenario: "SUCCESS", ExpiresAt: tc.instant.Add(time.Hour)})
			if err != nil {
				t.Fatal("boundary order creation failed")
			}
			paid, err := r.Complete(ctx, r.paymentToken(o.MerchantOrderNo))
			if err != nil {
				t.Fatal("boundary payment failed")
			}
			server := httptest.NewServer(NewService(r).Handler(strings.Repeat("s", 32)))
			defer server.Close()
			provider, err := paymentsimulator.New(server.URL, strings.Repeat("s", 32))
			if err != nil {
				t.Fatal("provider construction failed")
			}
			rows, err := provider.Bill(ctx, "ALIPAY", day)
			if err != nil {
				t.Fatal("production HTTP bill failed")
			}
			if len(rows) != tc.count {
				t.Fatalf("Shanghai half-open day rows=%d expected=%d", len(rows), tc.count)
			}
			if tc.count == 1 && (rows[0].EntryType != "PAYMENT" || rows[0].ProviderReferenceID != *paid.ChannelTransactionID || rows[0].Status != "SUCCESS" || rows[0].AmountFen != 100 || !rows[0].PaidAt.Equal(tc.instant)) {
				t.Fatal("payment bill lost immutable payment facts")
			}
		})
	}
}

func TestPostgresSimulatorBillIndependentPaymentAndRefund(t *testing.T) {
	for _, scenario := range []string{"SUCCESS", "REFUND_FAIL"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 10, 7, 8, 0, 0, 0, time.UTC)
			r := &PostgresRepository{pool: simulatorDatabase(t), publicURL: "https://controlled-simulator.invalid", tokenSecret: []byte(strings.Repeat("t", 32)), hmacSecrets: map[string][]byte{"ALIPAY": []byte(strings.Repeat("s", 32))}, now: func() time.Time { return now }}
			o, _, err := r.Create(ctx, CreateOrder{MerchantOrderNo: uuid.NewString(), Channel: "ALIPAY", AmountFen: 100, Currency: "CNY", CallbackURL: "http://127.0.0.1/controlled-unused", Scenario: scenario, ExpiresAt: now.Add(time.Hour)})
			if err != nil {
				t.Fatal("refund fixture creation failed")
			}
			paid, err := r.Complete(ctx, r.paymentToken(o.MerchantOrderNo))
			if err != nil {
				t.Fatal("payment failed")
			}
			now = now.Add(time.Hour)
			key := uuid.NewString()
			refund, created, err := r.Refund(ctx, o.MerchantOrderNo, key, 100)
			if err != nil || !created {
				t.Fatal("refund creation failed")
			}
			replay, recreated, err := r.Refund(ctx, o.MerchantOrderNo, key, 100)
			if err != nil || recreated || replay.MerchantRefundNo != refund.MerchantRefundNo || replay.ChannelRefundID != refund.ChannelRefundID || replay.AmountFen != refund.AmountFen || replay.Status != refund.Status || !replay.UpdatedAt.Equal(refund.UpdatedAt) {
				t.Fatal("refund immutable same-key replay failed")
			}
			server := httptest.NewServer(NewService(r).Handler(strings.Repeat("s", 32)))
			defer server.Close()
			provider, err := paymentsimulator.New(server.URL, strings.Repeat("s", 32))
			if err != nil {
				t.Fatal("provider construction failed")
			}
			day := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
			rows, err := provider.Bill(ctx, "ALIPAY", day)
			if err != nil {
				t.Fatal("refund bill HTTP failed")
			}
			want := 1
			if scenario == "SUCCESS" {
				want = 2
			}
			if len(rows) != want {
				t.Fatalf("independent payment/refund rows=%d expected=%d", len(rows), want)
			}
			if rows[0].EntryType != "PAYMENT" || rows[0].ProviderReferenceID != *paid.ChannelTransactionID || rows[0].Status != "SUCCESS" || rows[0].AmountFen != 100 || !rows[0].PaidAt.Equal(*paid.PaidAt) {
				t.Fatal("refund overwrote original payment bill")
			}
			if want == 2 && (rows[1].EntryType != "REFUND" || rows[1].ProviderReferenceID != refund.ChannelRefundID || rows[1].ChannelTransactionID != *paid.ChannelTransactionID || rows[1].Status != "SUCCESS" || rows[1].AmountFen != 100 || !rows[1].PaidAt.Equal(refund.UpdatedAt)) {
				t.Fatal("successful refund bill missing actual immutable refund facts")
			}
			var count int
			if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM simulator_refunds`).Scan(&count); err != nil || count != 1 {
				t.Fatal("same-key refund duplicated durable facts")
			}
			// The refund belongs to its own Shanghai business day, independent of
			// the original payment's paid_at, including a different UTC date.
			if scenario == "SUCCESS" {
				second, _, createErr := r.Create(ctx, CreateOrder{MerchantOrderNo: uuid.NewString(), Channel: "ALIPAY", AmountFen: 100, Currency: "CNY", CallbackURL: "http://127.0.0.1/controlled-unused", Scenario: "SUCCESS", ExpiresAt: now.Add(time.Hour)})
				if createErr != nil {
					t.Fatal("next-day refund order failed")
				}
				if _, err = r.Complete(ctx, r.paymentToken(second.MerchantOrderNo)); err != nil {
					t.Fatal("next-day refund payment failed")
				}
				now = time.Date(2026, 10, 7, 16, 0, 0, 0, time.UTC)
				later, _, err := r.Refund(ctx, second.MerchantOrderNo, uuid.NewString(), 100)
				if err != nil {
					t.Fatal("next-day production refund failed")
				}
				rows, err = provider.Bill(ctx, "ALIPAY", day)
				if err != nil || len(rows) != 3 {
					t.Fatal("next-day refund leaked into payment business day")
				}
				rows, err = provider.Bill(ctx, "ALIPAY", day.AddDate(0, 0, 1))
				if err != nil || len(rows) != 1 || rows[0].EntryType != "REFUND" || rows[0].ProviderReferenceID != later.ChannelRefundID {
					t.Fatal("next-day refund lost its own business date")
				}

			}
		})
	}
}
