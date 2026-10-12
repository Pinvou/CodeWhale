package billing

import (
	"context"
	"github.com/Pinvou/pinvou3/account-platform/internal/paymentsim"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/paymentsimulator"
	"github.com/Pinvou/pinvou3/account-platform/internal/simulatorschema"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// Only the bill attribution is corrupted; all other provider calls still use
// the actual protected Sim HTTP adapter and its real stored payment/refund.
type f03WrongMerchantBillProvider struct {
	PaymentProvider
	rows []ProviderBillEntry
}

func (p f03WrongMerchantBillProvider) Bill(context.Context, string, time.Time) ([]ProviderBillEntry, error) {
	return append([]ProviderBillEntry(nil), p.rows...), nil
}

// Actual isolated Sim Postgres + protected Sim HTTP/adapter + Billing/ledger
// transactions. Callback delivery is intentionally absent: Bill/OrderQuery
// must recover the real frozen customer/SKU, then reconcile the real refund.
// This is local simulator integration, not a live supplier or full E2E.
func TestF03SimulatorBillRefundAndLedger(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	r, ctx := retainedFinanceRepository(t)
	_, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	const sku = "controlled-real-mapped-sku"
	const secret = "public-fixture-hmac-key-at-least-32-bytes"
	const token = "public-fixture-simulator-service-token"
	order.ProviderID = "development_simulator"
	order.ProviderVersion = "development-simulator-v1"
	order.ProviderSKU = sku
	if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET provider_id=$2,provider_version=$3,provider_sku=$4,product_mapping_snapshot='{"status":"TEST_ONLY"}'::jsonb WHERE id=$1`, order.ID, order.ProviderID, order.ProviderVersion, sku); err != nil {
		t.Fatal(err)
	}
	raw := os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL")
	configuration, err := pgx.ParseConfig(raw)
	if err != nil {
		t.Fatal("controlled connection configuration invalid")
	}
	schema := "retained_sim_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin, err := pgx.ConnectConfig(ctx, configuration)
	if err != nil {
		t.Fatal("simulator fixture unavailable")
	}
	quoted := pgx.Identifier{schema}.Sanitize()
	var exists bool
	if err = admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)`, schema).Scan(&exists); err != nil || exists {
		admin.Close(ctx)
		t.Fatal("owned simulator schema identity guard failed")
	}
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		admin.Close(ctx)
		t.Fatal("owned simulator schema creation failed")
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error("owned simulator schema cleanup failed")
		}
		var remains bool
		if err := admin.QueryRow(cleanup, `SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)`, schema).Scan(&remains); err != nil || remains {
			t.Error("owned simulator schema remains")
		}
		admin.Close(cleanup)
	})
	configuration.RuntimeParams["search_path"] = schema
	setup, err := pgx.ConnectConfig(ctx, configuration)
	if err != nil {
		t.Fatal("owned simulator schema unavailable")
	}
	loaded, err := migrations.Load("../../migrations/payment-simulator")
	if err == nil {
		err = simulatorschema.Up(ctx, setup, loaded)
	}
	setup.Close(ctx)
	if err != nil {
		t.Fatal("production simulator schema failed")
	}
	secrets := map[string]string{"UNIONPAY": secret, "ALIPAY": secret, "WECHAT": secret}
	simulatorURL, err := url.Parse(raw)
	if err != nil {
		t.Fatal("controlled simulator URL invalid")
	}
	query := simulatorURL.Query()
	query.Set("search_path", schema)
	simulatorURL.RawQuery = query.Encode()
	sim, err := paymentsim.NewPostgresRepository(ctx, simulatorURL.String(), "http://127.0.0.1", "public-fixture-pay-token-at-least-32-bytes", secrets)
	if err != nil {
		t.Fatal("production simulator store failed")
	}
	t.Cleanup(sim.Close)
	simService := paymentsim.NewService(sim)
	upstream := httptest.NewServer(simService.Handler(token))
	t.Cleanup(upstream.Close)
	provider, err := paymentsimulator.New(upstream.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewPaymentCoordinatorWithProvider(r, provider, "test")
	if err != nil {
		t.Fatal(err)
	}
	receiver := httptest.NewServer(NewService(r).WithPayments(coordinator).Handler(token))
	t.Cleanup(receiver.Close)
	callback := receiver.URL + "/internal/payment/notifications"
	if err = coordinator.ConfigureNotifications("pinvou-development", callback, secrets); err != nil {
		t.Fatal(err)
	}
	created, err := provider.Create(ctx, payment.Order{MerchantOrderNo: order.MerchantOrderNo, MerchantCustomerKey: order.RequesterID.String(), Mobile: "+8613800138000", ExternalSKU: sku, Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency}, callback)
	if err != nil {
		t.Fatal(err)
	}
	paymentURL, err := url.Parse(created.PaymentURL)
	if err != nil {
		t.Fatal("simulator public fixture URL invalid")
	}
	completion, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL+paymentURL.Path+"/complete", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(completion)
	if err != nil {
		t.Fatal("public simulator completion unavailable")
	}
	io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal("public simulator completion rejected")
	}
	actual, err := provider.Get(ctx, order.MerchantOrderNo)
	if err != nil || actual.ExternalCustomerKey != order.RequesterID.String() || actual.ExternalSKU != sku || actual.Status != "SUCCESS" {
		t.Fatal("actual authenticated provider mapping facts failed", err)
	}
	local := actual.PaidAt.In(time.FixedZone("Asia/Shanghai", 8*60*60))
	date := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	actor := uuid.New()
	run, err := coordinator.RunAdminReconciliation(ctx, actor, order.Channel, date)
	if err != nil || run.CaseCount != 0 || run.ProviderRowCount != 1 {
		t.Fatal("actual bill/query did not recover matching payment", err)
	}
	wallet, err := r.GetWallet(ctx, order.WalletID)
	if err != nil || wallet.PostedMicrocredits != 1000000 {
		t.Fatal("actual bill recovery did not credit frozen wallet", err)
	}
	var transaction, pack uuid.UUID
	if err = r.pool.QueryRow(ctx, `SELECT t.id,p.id FROM payment_transactions t JOIN entitlement_packages p ON p.source_payment_transaction_id=t.id WHERE t.payment_order_id=$1`, order.ID).Scan(&transaction, &pack); err != nil {
		t.Fatal(err)
	}
	coordinator.AllowNormalRefundCreation(true)
	key, reason := uuid.NewString(), "controlled whole source refund"
	refund, fresh, err := coordinator.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, key, reason)
	if err != nil || !fresh || refund.AmountFen != order.AmountFen {
		t.Fatal("actual whole refund reservation failed", err)
	}
	if err = coordinator.ProcessRefund(ctx, refund.ID); err != nil {
		t.Fatal("actual simulator refund failed", err)
	}
	if err = coordinator.ProcessRefund(ctx, refund.ID); err != nil {
		t.Fatal("refund processing replay failed", err)
	}
	if replay, fresh, err := coordinator.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, key, reason); err != nil || fresh || replay.ID != refund.ID || replay.Status != "SUCCEEDED" {
		t.Fatal("whole refund identity did not replay", err)
	}
	run, err = coordinator.RunAdminReconciliation(ctx, actor, order.Channel, date)
	if err != nil || run.CaseCount != 0 || run.ProviderRowCount != 2 {
		t.Fatal("actual payment/refund/ledger four-way mismatch", err)
	}
	rows, err := provider.Bill(ctx, order.Channel, date)
	if err != nil || len(rows) != 2 || rows[0].ProviderReferenceID == rows[1].ProviderReferenceID {
		t.Fatal("payment/refund references are not independent", err)
	}
	wallet, err = r.GetWallet(ctx, order.WalletID)
	var payments, refunds int
	if err != nil || wallet.PostedMicrocredits != 0 || wallet.ReservedMicrocredits != 0 {
		t.Fatal("refund changed exact wallet facts", err)
	}
	if err = r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM journal_entries WHERE entry_type='RECHARGE'),(SELECT count(*) FROM journal_entries WHERE entry_type='REFUND')`).Scan(&payments, &refunds); err != nil || payments != 1 || refunds != 1 {
		t.Fatal("bill/refund replay duplicated ledger effect", err)
	}
	for index := range rows {
		rows[index].MerchantOrderNo = "wrong-" + order.MerchantOrderNo
	}
	wrongCoordinator, err := NewPaymentCoordinatorWithProvider(r, f03WrongMerchantBillProvider{PaymentProvider: provider, rows: rows}, "test")
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := wrongCoordinator.RunAdminReconciliation(ctx, actor, order.Channel, date)
	if err != nil || wrong.CaseCount != 4 {
		t.Fatal("real payment/refund references with wrong order did not expose both missing sides", err)
	}
	counts := map[string]int{}
	cases, err := r.AdminReconciliationCases(ctx, wrong.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range cases {
		counts[item.CaseType]++
	}
	for _, kind := range []string{"INTERNAL_PAID_PROVIDER_MISSING", "PROVIDER_PAID_INTERNAL_MISSING", "INTERNAL_REFUND_PROVIDER_MISSING", "PROVIDER_REFUND_INTERNAL_MISSING"} {
		if counts[kind] != 1 {
			t.Fatal("wrong-order bill omitted an existing missing case", counts)
		}
	}
	wallet, err = r.GetWallet(ctx, order.WalletID)
	if err != nil || wallet.PostedMicrocredits != 0 || wallet.ReservedMicrocredits != 0 {
		t.Fatal("wrong-order bill changed refunded wallet", err)
	}
	if err = r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM journal_entries WHERE entry_type='RECHARGE'),(SELECT count(*) FROM journal_entries WHERE entry_type='REFUND')`).Scan(&payments, &refunds); err != nil || payments != 1 || refunds != 1 {
		t.Fatal("wrong-order bill changed real ledger effects", err)
	}
}
