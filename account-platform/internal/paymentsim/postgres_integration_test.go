package paymentsim

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
)

func simulatorFixture(t *testing.T, scenario string) (*PostgresRepository, Order) {
	t.Helper()
	r := &PostgresRepository{pool: testdb.Open(t, "payment-simulator"), publicURL: "https://simulator.example", tokenSecret: []byte("test-only-token-secret-thirty-two-chars"), hmacSecrets: map[string][]byte{"ALIPAY": []byte("test-only-hmac-secret-thirty-two-chars")}, now: func() time.Time { return time.Now().UTC() }}
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

func TestPostgresSimulatorToBillingSignedCallbackAndDelayedRetry(t *testing.T) {
	for _, channel := range []string{"ALIPAY", "UNIONPAY", "WECHAT"} {
		t.Run(channel, func(t *testing.T) {
			ctx := context.Background()
			simPool := testdb.Open(t, "payment-simulator")
			billingPool := testdb.Open(t, "billing")
			now := time.Now().UTC()
			secret := "test-only-payment-hmac-thirty-two-chars"
			secrets := map[string]string{"ALIPAY": secret, "UNIONPAY": secret, "WECHAT": secret}
			parsedSecrets := map[string][]byte{"ALIPAY": []byte(secret), "UNIONPAY": []byte(secret), "WECHAT": []byte(secret)}
			serviceToken := strings.Repeat("t", 32)
			sim := &PostgresRepository{pool: simPool, tokenSecret: []byte("test-only-token-secret-thirty-two-chars"), hmacSecrets: parsedSecrets, now: func() time.Time { return now }}
			simService := NewService(sim)
			simServer := httptest.NewServer(simService.Handler(serviceToken))
			defer simServer.Close()
			sim.publicURL = simServer.URL
			billingConfig := billingPool.Config()
			databaseURL, err := url.Parse(billingConfig.ConnString())
			if err != nil {
				t.Fatal(err)
			}
			query := databaseURL.Query()
			query.Set("search_path", billingConfig.ConnConfig.RuntimeParams["search_path"])
			databaseURL.RawQuery = query.Encode()
			repo, err := billing.NewPostgresRepository(ctx, databaseURL.String())
			if err != nil {
				t.Fatal(err)
			}
			defer repo.Close()
			billingServer := httptest.NewUnstartedServer(nil)
			coordinator, err := billing.NewPaymentCoordinator(repo, simServer.URL, serviceToken, "pinvou-development", "http://"+billingServer.Listener.Addr().String()+"/internal/payment/notifications", secrets)
			if err != nil {
				t.Fatal(err)
			}
			// The callback server initially returns a lost acknowledgement after
			// Billing has committed. Its next delivery uses the same notification ID.
			handler := billing.NewService(repo).WithPayments(coordinator).Handler(serviceToken)
			lostAck := true
			billingServer.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if lostAck {
					lostAck = false
					buffer := httptest.NewRecorder()
					handler.ServeHTTP(buffer, r)
					if buffer.Code != http.StatusOK {
						t.Errorf("first callback rejected: %d %s", buffer.Code, buffer.Body.String())
					}
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				handler.ServeHTTP(w, r)
			})
			billingServer.Start()
			defer billingServer.Close()
			// Keep exercising ordinary CNY credit and callback recovery through
			// the retained organization catalog, rather than a retired personal SKU.
			wallet, _, err := repo.EnsureWallet(ctx, "ORGANIZATION", uuid.New())
			if err != nil {
				t.Fatal(err)
			}
			order, _, err := coordinator.CreateOrder(ctx, uuid.New(), wallet.ID, "CNY_1", channel, "simulator-http-payment-key")
			if err != nil {
				t.Fatal(err)
			}
			payment, err := http.Post(simServer.URL+"/pay/"+sim.paymentToken(order.MerchantOrderNo)+"/complete", "application/json", strings.NewReader(""))
			if err != nil {
				t.Fatal(err)
			}
			responseBody, _ := io.ReadAll(payment.Body)
			payment.Body.Close()
			if payment.StatusCode != http.StatusOK {
				t.Fatalf("phone payment %d %s", payment.StatusCode, responseBody)
			}
			// Simulate a queue entry whose original delivery signature expired,
			// retaining the actual payment time and all financial facts.
			if _, err := simPool.Exec(ctx, `UPDATE simulator_notifications SET payload=convert_to(jsonb_set(convert_from(payload,'UTF8')::jsonb,'{timestamp}',to_jsonb($1::bigint))::text,'UTF8'),next_attempt_at=$2`, now.Add(-10*time.Minute).Unix(), now.Add(-time.Second)); err != nil {
				t.Fatal(err)
			}
			if err := simService.DeliverPending(ctx); err != nil {
				t.Fatal(err)
			}
			credited, err := repo.GetWallet(ctx, wallet.ID)
			if err != nil || credited.PostedMicrocredits != order.CreditMicrocredits {
				t.Fatalf("normal callback failed %+v %v", credited, err)
			}
			var notificationID uuid.UUID
			var attempts int
			var state string
			if err := simPool.QueryRow(ctx, `SELECT notification_id,attempts,status FROM simulator_notifications`).Scan(&notificationID, &attempts, &state); err != nil || attempts != 1 || state != "PENDING" {
				t.Fatalf("lost acknowledgement %v %d %s %v", notificationID, attempts, state, err)
			}
			// The refreshed retry authenticates a new delivery of the same event.
			now = time.Now().UTC().Add(time.Second)
			if _, err := simPool.Exec(ctx, `UPDATE simulator_notifications SET next_attempt_at=$1`, now.Add(-time.Second)); err != nil {
				t.Fatal(err)
			}
			if err := simService.DeliverPending(ctx); err != nil {
				t.Fatal(err)
			}
			var finalID uuid.UUID
			if err := simPool.QueryRow(ctx, `SELECT notification_id,attempts,status FROM simulator_notifications`).Scan(&finalID, &attempts, &state); err != nil || finalID != notificationID || attempts != 2 || state != "DELIVERED" {
				t.Fatalf("retry changed event %v %v %d %s %v", notificationID, finalID, attempts, state, err)
			}
			var credits, events, notifications int
			if err := billingPool.QueryRow(ctx, `SELECT (SELECT count(*) FROM journal_entries WHERE business_reference=$1),(SELECT count(*) FROM outbox_events WHERE aggregate_id=$2),(SELECT count(*) FROM payment_notifications WHERE notification_id=$3)`, "recharge:"+order.ID.String(), order.ID, notificationID).Scan(&credits, &events, &notifications); err != nil || credits != 1 || events != 1 || notifications != 1 {
				t.Fatalf("duplicate financial effects: %d %d %d %v", credits, events, notifications, err)
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

func TestPostgresPaymentActivationRecoversProviderTerminalOverHTTP(t *testing.T) {
	for _, status := range []string{"CLOSED", "FAILED"} {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			sim, _ := simulatorFixture(t, "SUCCESS")
			token := strings.Repeat("s", 32)
			handler := NewService(sim).Handler(token)
			loseResponse := true
			simServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if loseResponse && r.Method == http.MethodPost && r.URL.Path == "/internal/v1/orders" {
					loseResponse = false
					buffer := httptest.NewRecorder()
					handler.ServeHTTP(buffer, r)
					if buffer.Code != http.StatusCreated {
						t.Errorf("provider create failed %d %s", buffer.Code, buffer.Body.String())
					}
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				handler.ServeHTTP(w, r)
			}))
			defer simServer.Close()
			sim.publicURL = simServer.URL
			pool := testdb.Open(t, "billing")
			cfg := pool.Config()
			dsn, _ := url.Parse(cfg.ConnString())
			query := dsn.Query()
			query.Set("search_path", cfg.ConnConfig.RuntimeParams["search_path"])
			dsn.RawQuery = query.Encode()
			repo, err := billing.NewPostgresRepository(ctx, dsn.String())
			if err != nil {
				t.Fatal(err)
			}
			defer repo.Close()
			secret := "test-only-payment-hmac-thirty-two-chars"
			coordinator, err := billing.NewPaymentCoordinator(repo, simServer.URL, token, "pinvou-development", "http://127.0.0.1/unused-notification", map[string]string{"ALIPAY": secret, "UNIONPAY": secret, "WECHAT": secret})
			if err != nil {
				t.Fatal(err)
			}
			wallet, _, err := repo.EnsureWallet(ctx, "ORGANIZATION", uuid.New())
			if err != nil {
				t.Fatal(err)
			}
			user := uuid.New()
			prepared, _, err := coordinator.CreateOrder(ctx, user, wallet.ID, "CNY_1", "ALIPAY", "activation-terminal-key")
			if !errors.Is(err, billing.ErrPaymentProvider) {
				t.Fatalf("response loss %v", err)
			}
			var request *http.Request
			if status == "CLOSED" {
				request, _ = http.NewRequest(http.MethodPost, simServer.URL+"/internal/v1/orders/"+prepared.MerchantOrderNo+"/close", strings.NewReader(`{}`))
				request.Header.Set("Authorization", "Bearer "+token)
			} else {
				if _, err := sim.pool.Exec(ctx, `UPDATE simulator_orders SET scenario='FAIL' WHERE merchant_order_no=$1`, prepared.MerchantOrderNo); err != nil {
					t.Fatal(err)
				}
				request, _ = http.NewRequest(http.MethodPost, simServer.URL+"/pay/"+sim.paymentToken(prepared.MerchantOrderNo)+"/complete", strings.NewReader(`{}`))
			}
			response, err := simServer.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("provider terminal %d", response.StatusCode)
			}
			if status == "FAILED" {
				// Fault injection exercised the real phone handler. Restore the
				// original creation scenario for a matching Create replay while
				// retaining its FAILED channel fact in this isolated fixture.
				if _, err := sim.pool.Exec(ctx, `UPDATE simulator_orders SET scenario='SUCCESS' WHERE merchant_order_no=$1`, prepared.MerchantOrderNo); err != nil {
					t.Fatal(err)
				}
			}
			recovered, created, err := coordinator.CreateOrder(ctx, user, wallet.ID, "CNY_1", "ALIPAY", "activation-terminal-key")
			if err != nil || created || recovered.ID != prepared.ID || recovered.Status != status {
				t.Fatalf("activation returned wrong terminal %+v %v %v", recovered, created, err)
			}
			stored, err := coordinator.GetOrder(ctx, prepared.ID, user)
			if err != nil || stored.Status != status {
				t.Fatalf("local terminal %+v %v", stored, err)
			}
		})
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
