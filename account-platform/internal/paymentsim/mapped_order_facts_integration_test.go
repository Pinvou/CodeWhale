package paymentsim

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/paymentsimulator"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
)

// The fixture uses the production provider factory/mappings and unchanged
// signed-notification protocol; it does not restore the retired coordinator API.
func newMappedSimulatorCoordinatorForTest(t *testing.T, r *billing.PostgresRepository, raw, token, callback string, secrets map[string]string) (*billing.PaymentCoordinator, error) {
	t.Helper()
	provider, err := paymentsimulator.New(raw, token)
	if err != nil {
		return nil, err
	}
	maps, err := r.ActiveExternalProducts(context.Background(), provider.Capabilities().ProviderID, provider.Capabilities().Version, true)
	if err != nil {
		return nil, err
	}
	c, err := billing.NewPaymentCoordinatorWithProvider(r, provider, maps, "test")
	if err != nil {
		return nil, err
	}
	c.AllowTentativePrices(true)
	return c, c.ConfigureNotifications("pinvou-development", callback, secrets)
}

func TestPostgresMappedSimulatorPersistsFactsOverHTTP(t *testing.T) {
	for _, owner := range []string{"ORGANIZATION", "PERSONAL"} {
		t.Run(owner, func(t *testing.T) {
			ctx := context.Background()
			secret := strings.Repeat("s", 32)
			sp := testdb.Open(t, "payment-simulator")
			sim, err := NewPostgresRepository(ctx, sp.Config().ConnString(), "https://controlled-simulator.invalid", secret, map[string]string{"ALIPAY": secret, "UNIONPAY": secret, "WECHAT": secret})
			if err != nil {
				t.Fatal(err)
			}
			defer sim.Close()
			server := httptest.NewServer(NewService(sim).Handler(secret))
			defer server.Close()
			sim.publicURL = server.URL
			bp := testdb.Open(t, "billing")
			repo, err := billing.NewPostgresRepository(ctx, bp.Config().ConnString())
			if err != nil {
				t.Fatal(err)
			}
			defer repo.Close()
			c, err := newMappedSimulatorCoordinatorForTest(t, repo, server.URL, secret, "http://127.0.0.1/controlled-unused", map[string]string{"ALIPAY": secret, "UNIONPAY": secret, "WECHAT": secret})
			if err != nil {
				t.Fatal(err)
			}
			user, id := uuid.New(), uuid.New()
			if owner == "PERSONAL" {
				id = user
			}
			wallet, _, err := repo.EnsureWallet(ctx, owner, id)
			if err != nil {
				t.Fatal(err)
			}
			product := "CNY_1"
			if owner == "PERSONAL" {
				product = "PLAN_TRIAL"
			}
			maps, err := repo.ActiveExternalProducts(ctx, "development_simulator", "development-simulator-v1", true)
			if err != nil {
				t.Fatal(err)
			}
			mobile := "+8613800000000"
			key := uuid.NewString()
			order, created, err := c.CreateMobileOrder(ctx, user, wallet.ID, product, "ALIPAY", mobile, key)
			if err != nil || !created || order.Status != "PENDING" {
				t.Fatalf("mapped real simulator rejected %v", err)
			}
			// A new repository/client reads actual durable fields after restart rather
			// than an adapter-local cache or fabricated request echo.
			restarted, err := NewPostgresRepository(ctx, sp.Config().ConnString(), server.URL, secret, map[string]string{"ALIPAY": secret, "UNIONPAY": secret, "WECHAT": secret})
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			peer := httptest.NewServer(NewService(restarted).Handler(secret))
			defer peer.Close()
			provider, err := paymentsimulator.New(peer.URL, secret)
			if err != nil {
				t.Fatal(err)
			}
			remote, err := provider.Get(ctx, order.MerchantOrderNo)
			if err != nil || remote.Mobile != mobile || remote.ProviderSKU != maps[product].SKU {
				t.Fatal("restart lost durable mapped request facts", err)
			}
			original := payment.Order{MerchantOrderNo: order.MerchantOrderNo, Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, Mobile: mobile, ExternalSKU: maps[product].SKU}
			for _, changed := range []payment.Order{func() payment.Order { x := original; x.Mobile = "+8613900000000"; return x }(), func() payment.Order { x := original; x.ExternalSKU += "-different"; return x }()} {
				if _, err := provider.Create(ctx, changed, "http://127.0.0.1/controlled-unused"); err == nil {
					t.Fatal("same merchant accepted changed immutable facts")
				}
			}
			replay, newOrder, err := c.CreateMobileOrder(ctx, user, wallet.ID, product, "ALIPAY", mobile, key)
			if err != nil || newOrder || replay.ID != order.ID {
				t.Fatal("same logical mapped order did not replay", err)
			}
			var count int
			if err = sp.QueryRow(ctx, `SELECT count(*) FROM simulator_orders`).Scan(&count); err != nil || count != 1 {
				t.Fatal("duplicate remote order", err)
			}
			page, err := server.Client().Get(remote.PaymentURL)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(page.Body)
			page.Body.Close()
			if err != nil || page.StatusCode != 200 || strings.Contains(string(body), mobile) || strings.Contains(string(body), maps[product].SKU) {
				t.Fatal("public payment page exposed mapped metadata", err)
			}
			paid, err := server.Client().Post(remote.PaymentURL+"/complete", "application/json", strings.NewReader(""))
			if err != nil {
				t.Fatal(err)
			}
			body, err = io.ReadAll(paid.Body)
			paid.Body.Close()
			if err != nil || paid.StatusCode != 200 || strings.Contains(string(body), mobile) || strings.Contains(string(body), maps[product].SKU) {
				t.Fatal("public completion exposed mapped metadata", err)
			}
			complete, err := provider.Get(ctx, order.MerchantOrderNo)
			if err != nil || complete.Status != "SUCCESS" || complete.Mobile != mobile || complete.ProviderSKU != maps[product].SKU {
				t.Fatal("completion lost protected internal facts", err)
			}
			var payload []byte
			if err = sp.QueryRow(ctx, `SELECT payload FROM simulator_notifications`).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			var notification map[string]json.RawMessage
			if json.Unmarshal(payload, &notification) != nil {
				t.Fatal("invalid existing signed payload")
			}
			if _, ok := notification["mobile"]; ok {
				t.Fatal("mobile added to notification contract")
			}
			if _, ok := notification["provider_sku"]; ok {
				t.Fatal("SKU added to notification contract")
			}
			// Closing a paid order remains forbidden. Exercise Close on a separate
			// pending order, preserving the existing terminal payment policy.
			pending := original
			pending.MerchantOrderNo = uuid.NewString()
			if _, err := provider.Create(ctx, pending, "http://127.0.0.1/controlled-unused"); err != nil {
				t.Fatal(err)
			}
			closed, err := provider.Close(ctx, pending.MerchantOrderNo)
			if err != nil || closed.Status != "CLOSED" || closed.Mobile != mobile || closed.ProviderSKU != maps[product].SKU {
				t.Fatal("pending close lost protected fields", err)
			}
		})
	}
}

func TestPostgresMappedSimulatorMigrationPreservesOldFacts(t *testing.T) {
	ctx := context.Background()
	pool := testdb.Open(t, "payment-simulator")
	down, err := os.ReadFile(filepath.Join("..", "..", "migrations", "payment-simulator", "000005_mapped_order_facts.down.sql"))
	if err != nil {
		t.Fatal(err)
	}
	up, err := os.ReadFile(filepath.Join("..", "..", "migrations", "payment-simulator", "000005_mapped_order_facts.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	// Actual 1..4 schema, then the new forward migration with a durable old row.
	if _, err = pool.Exec(ctx, string(down)); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	expires := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	if _, err = pool.Exec(ctx, `INSERT INTO simulator_orders(id,merchant_order_no,channel,amount_fen,currency,status,scenario,pay_token_hash,callback_url,expires_at,created_at,updated_at) VALUES($1,'controlled-old-order','ALIPAY',100,'CNY','PENDING','SUCCESS',$2,'http://127.0.0.1/controlled-unused',$3,$4,$4)`, id, []byte("controlled-old-hash"), expires, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var before json.RawMessage
	if err = pool.QueryRow(ctx, `SELECT to_jsonb(simulator_orders) FROM simulator_orders WHERE id=$1`, id).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, string(up)); err != nil {
		t.Fatal(err)
	}
	var same bool
	if err = pool.QueryRow(ctx, `SELECT (to_jsonb(simulator_orders)-'mobile'-'provider_sku')=$2::jsonb AND mobile='' AND provider_sku='' FROM simulator_orders WHERE id=$1`, id, before).Scan(&same); err != nil || !same {
		t.Fatal("old fields changed or unrecorded metadata invented", err)
	}
	r := &PostgresRepository{pool: pool, publicURL: "https://controlled-simulator.invalid", tokenSecret: []byte(strings.Repeat("t", 32)), now: time.Now}
	old, err := r.GetByMerchantOrder(ctx, "controlled-old-order")
	if err != nil || old.Mobile != "" || old.ProviderSKU != "" || !old.ExpiresAt.Equal(expires) {
		t.Fatal("old order did not remain readable", err)
	}
	if _, err = pool.Exec(ctx, string(down)); err != nil {
		t.Fatal("unknown old facts blocked safe rollback", err)
	}
	if _, err = pool.Exec(ctx, string(up)); err != nil {
		t.Fatal(err)
	}
	request := CreateOrder{MerchantOrderNo: "controlled-new-mapped", Channel: "ALIPAY", AmountFen: 100, Currency: "CNY", Mobile: "+8613800000000", ProviderSKU: "controlled-sku", Scenario: "SUCCESS", CallbackURL: "http://127.0.0.1/controlled-unused", ExpiresAt: expires}
	newOrder, created, err := r.Create(ctx, request)
	if err != nil || !created {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, string(down)); err == nil {
		t.Fatal("rollback discarded mapped immutable facts")
	}
	saved, err := r.GetByMerchantOrder(ctx, newOrder.MerchantOrderNo)
	if err != nil || saved.Mobile != request.Mobile || saved.ProviderSKU != request.ProviderSKU {
		t.Fatal("refused rollback changed order facts", err)
	}
}

func TestPostgresMappedSimulatorRollbackFencesConcurrentCreate(t *testing.T) {
	ctx := context.Background()
	pool := testdb.Open(t, "payment-simulator")
	raw, err := os.ReadFile(filepath.Join("..", "..", "migrations", "payment-simulator", "000005_mapped_order_facts.down.sql"))
	if err != nil {
		t.Fatal(err)
	}
	sql := string(raw)
	at := strings.Index(sql, "ALTER TABLE simulator_orders DROP COLUMN")
	if at < 0 {
		t.Fatal("actual rollback DDL missing")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// Split the actual migration only at its legal interstatement schedule.
	// The first statement must exclude writers before checking durable facts.
	if _, err = tx.Exec(ctx, sql[:at]); err != nil {
		t.Fatal(err)
	}
	r := &PostgresRepository{pool: pool, publicURL: "https://controlled-simulator.invalid", tokenSecret: []byte(strings.Repeat("t", 32)), now: time.Now}
	createCtx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	_, created, createErr := r.Create(createCtx, CreateOrder{MerchantOrderNo: "controlled-concurrent-rollback", Channel: "ALIPAY", AmountFen: 100, Currency: "CNY", Mobile: "+8613800000000", ProviderSKU: "controlled-sku", Scenario: "SUCCESS", CallbackURL: "http://127.0.0.1/controlled-unused", ExpiresAt: time.Now().Add(time.Hour)})
	if createErr == nil || created {
		t.Fatal("rollback permitted acknowledgement of a concurrent mapped order")
	}
	if _, err = tx.Exec(ctx, sql[at:]); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var rows, columns int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM simulator_orders WHERE merchant_order_no='controlled-concurrent-rollback'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='simulator_orders' AND column_name IN ('mobile','provider_sku')`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || columns != 0 {
		t.Fatal("rollback failed to fence the writer or drop the empty schema")
	}
}
