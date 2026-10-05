package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/paymentsim"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/Pinvou/pinvou3/account-platform/internal/token"
	"github.com/google/uuid"
)

type organizationContextFixture struct {
	mu      sync.Mutex
	context token.AuthorizationContext
}

func (f *organizationContextFixture) Resolve(context.Context, string) (token.AuthorizationContext, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.context, nil
}

func TestPostgresHTTPOrganizationLegacyRechargeAndPersonalPlanBoundaries(t *testing.T) {
	ctx := context.Background()
	secret := strings.Repeat("s", 32)
	r := &PostgresRepository{pool: testdb.Open(t, "billing"), now: func() time.Time { return time.Now().UTC() }}
	simPool := testdb.Open(t, "payment-simulator")
	dsn, err := url.Parse(simPool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	q := dsn.Query()
	q.Set("search_path", simPool.Config().ConnConfig.RuntimeParams["search_path"])
	dsn.RawQuery = q.Encode()
	var simHandler http.Handler
	simServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { simHandler.ServeHTTP(w, req) }))
	defer simServer.Close()
	sim, err := paymentsim.NewPostgresRepository(ctx, dsn.String(), simServer.URL, secret, map[string]string{"UNIONPAY": secret, "ALIPAY": secret, "WECHAT": secret})
	if err != nil {
		t.Fatal(err)
	}
	defer sim.Close()
	simHandler = paymentsim.NewService(sim).Handler(secret)
	p, err := NewPaymentCoordinator(r, simServer.URL, secret, "controlled-merchant", "http://127.0.0.1/controlled-callback", map[string]string{"UNIONPAY": secret, "ALIPAY": secret, "WECHAT": secret})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewService(r).WithPayments(p).Handler(secret))
	defer server.Close()
	client, err := token.NewHTTPBillingClient(server.URL, secret)
	if err != nil {
		t.Fatal(err)
	}
	wallets, err := token.NewHTTPWalletResolver(server.URL, secret)
	if err != nil {
		t.Fatal(err)
	}
	user, org := uuid.New(), uuid.New()
	contexts := &organizationContextFixture{context: token.AuthorizationContext{UserID: user, ContextType: "ORGANIZATION", OrganizationID: &org, Roles: []string{"OWNER"}}}
	gateway := httptest.NewServer(token.NewService(nil, contexts, wallets, nil, token.ProviderURLPolicy{}, client, nil, 1).Handler(func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Subject: "controlled", SessionID: "controlled"}, nil
	}))
	defer gateway.Close()
	request := func(path string, body any, key string) (int, []byte) {
		t.Helper()
		method := "GET"
		var input io.Reader
		if body != nil {
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			input = bytes.NewReader(encoded)
			method = "POST"
		}
		req, err := http.NewRequest(method, gateway.URL+path, input)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer controlled")
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, data
	}
	status, data := request("/v1/recharge/products", nil, "")
	var catalog struct {
		Items []Product `json:"items"`
	}
	if status != 200 || json.Unmarshal(data, &catalog) != nil || len(catalog.Items) != 4 {
		t.Fatal("original organization catalog was not preserved")
	}
	for _, product := range catalog.Items {
		if !originalOrganizationProduct(product) {
			t.Fatal("organization catalog exposed a personal or changed product")
		}
	}
	body := map[string]any{"product_id": "CNY_1", "channel": "ALIPAY"}
	key := uuid.NewString()
	status, data = request("/v1/recharge/orders", body, key)
	var order PaymentOrder
	if status != 201 || json.Unmarshal(data, &order) != nil || order.ProductID != "CNY_1" {
		t.Fatal("organization owner could not buy legacy credit")
	}
	status, replay := request("/v1/recharge/orders", body, key)
	var repeated PaymentOrder
	if status != 201 || json.Unmarshal(replay, &repeated) != nil || repeated.ID != order.ID {
		t.Fatal("legacy logical key replay changed order")
	}
	response, err := http.Post(order.PaymentURL+"/complete", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("real simulator payment failed")
	}
	paid, err := p.SyncOrder(ctx, order.ID)
	if err != nil || paid.FulfillmentStatus != "CREDITED" {
		t.Fatal("organization payment did not credit")
	}
	if _, err := p.SyncOrder(ctx, order.ID); err != nil {
		t.Fatal(err)
	}
	wallet, err := r.GetWallet(ctx, order.WalletID)
	if err != nil || wallet.PostedMicrocredits != 1000000 {
		t.Fatal("organization wallet credited more than once")
	}
	var packages int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM entitlement_packages WHERE wallet_id=$1`, order.WalletID).Scan(&packages); err != nil || packages != 0 {
		t.Fatal("organization legacy recharge issued personal package")
	}
	for _, role := range []string{"BILLING_ADMIN", "MEMBER"} {
		contexts.mu.Lock()
		contexts.context.Roles = []string{role}
		contexts.mu.Unlock()
		status, _ := request("/v1/recharge/orders", body, uuid.NewString())
		expected := 201
		if role == "MEMBER" {
			expected = 403
		}
		if status != expected {
			t.Fatal("original organization role boundary regressed")
		}
	}
	contexts.mu.Lock()
	contexts.context.Roles = []string{"OWNER"}
	contexts.mu.Unlock()
	if status, _ := request("/v1/recharge/orders", map[string]any{"product_id": "PLAN_TRIAL", "channel": "ALIPAY"}, uuid.NewString()); status != 403 {
		t.Fatal("organization acquired personal plan")
	}
	if _, _, err := p.CreateOrder(ctx, user, order.WalletID, "PLAN_TRIAL", "ALIPAY", uuid.NewString()); err == nil {
		t.Fatal("internal billing bypassed personal plan ownership")
	}
	personal, _, err := r.EnsureWallet(ctx, "PERSONAL", user)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.CreateOrder(ctx, user, personal.ID, "CNY_1", "ALIPAY", uuid.NewString()); err == nil {
		t.Fatal("retired personal legacy product reopened")
	}
	if _, err := r.pool.Exec(ctx, `INSERT INTO recharge_products(product_id,amount_fen,credit_microcredits,display_points,currency,channels,catalog_version,enabled,created_at) VALUES('OTHER_DISABLED',100,1000000,100,'CNY',ARRAY['ALIPAY'],1,false,now())`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.CreateOrder(ctx, user, order.WalletID, "OTHER_DISABLED", "ALIPAY", uuid.NewString()); err == nil {
		t.Fatal("arbitrary disabled organization SKU reopened")
	}
	if _, err := r.pool.Exec(ctx, `UPDATE recharge_products SET amount_fen=101 WHERE product_id='CNY_1'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.CreateOrder(ctx, user, order.WalletID, "CNY_1", "ALIPAY", uuid.NewString()); err == nil {
		t.Fatal("changed legacy amount bypassed frozen organization facts")
	}
	if recovered, _, err := p.CreateOrder(ctx, user, order.WalletID, "CNY_1", "ALIPAY", key); err != nil || recovered.ID != order.ID {
		t.Fatal("historical organization snapshot did not survive current catalog change")
	}
	assertBalanced(t, r)
}
