package billing

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

func TestPostgresAdminFinancialReadsKeepExactDecimalStringsAndOldBalanceNumbers(t *testing.T) {
	ctx := context.Background()
	repo, wallet := billingFixture(t)
	const amount int64 = 9007199254740993
	if _, err := repo.Credit(ctx, wallet.ID, "admin-money-fixture", amount+10, true); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Reserve(ctx, uuid.New(), wallet.ID, amount, nil, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RequestAdjustment(ctx, wallet.ID, -amount, "fixture", uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.pool.Exec(ctx, `INSERT INTO payment_orders(id,merchant_order_no,requester_id,wallet_id,product_id,product_snapshot,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,'fixture','{}','ALIPAY',$5,'CNY',$5,'CREATED','PENDING',now()+interval '1 hour',now(),now())`, uuid.New(), uuid.NewString(), uuid.New(), wallet.ID, amount); err != nil {
		t.Fatal(err)
	}
	serviceToken := strings.Repeat("f", 32)
	server := httptest.NewServer(NewService(repo).Handler(serviceToken, func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{RealmRoles: []string{"platform-admin"}}, nil
	}))
	defer server.Close()
	read := func(path string) map[string]any {
		t.Helper()
		request, _ := http.NewRequest(http.MethodGet, server.URL+path, nil)
		request.Header.Set("Authorization", "Bearer "+serviceToken)
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var body map[string]any
		decoder := json.NewDecoder(response.Body)
		decoder.UseNumber()
		if err := decoder.Decode(&body); err != nil || response.StatusCode != http.StatusOK {
			t.Fatal("financial HTTP fixture failed")
		}
		return body
	}
	for _, test := range []struct {
		path   string
		fields map[string]string
	}{
		{"/admin/v1/overview", map[string]string{"posted_microcredits": "9007199254741003", "reserved_microcredits": "9007199254740993"}},
		{"/admin/v1/wallets", map[string]string{"posted_microcredits": "9007199254741003", "reserved_microcredits": "9007199254740993", "available_microcredits": "10"}},
		{"/admin/v1/payment-orders", map[string]string{"amount_fen": "9007199254740993", "credit_microcredits": "9007199254740993"}},
		{"/admin/v1/adjustments", map[string]string{"amount_microcredits": "-9007199254740993"}},
		{"/admin/v1/usage-holds", map[string]string{"max_microcredits": "9007199254740993"}},
	} {
		t.Run(test.path, func(t *testing.T) {
			body := read(test.path)
			if items, ok := body["items"].([]any); ok {
				if len(items) != 1 {
					t.Fatal("missing financial record")
				}
				body = items[0].(map[string]any)
			}
			for field, want := range test.fields {
				if body[field] != want {
					t.Fatal("new admin money response was not an exact decimal string")
				}
			}
		})
	}
	body := read("/internal/v1/wallets/" + wallet.ID.String() + "/balance")
	if body["posted_microcredits"] != json.Number("9007199254741003") {
		t.Fatal("existing internal balance wire contract changed")
	}
	assertBalanced(t, repo)
}

func TestPostgresAdminOverviewTotalsMayExceedOneWalletInt64(t *testing.T) {
	ctx := context.Background()
	repo, first := billingFixture(t)
	second, _, err := repo.EnsureWallet(ctx, "PERSONAL", uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		wallet Wallet
		amount int64
	}{{first, math.MaxInt64}, {second, 1}} {
		if _, err := repo.Credit(ctx, item.wallet.ID, uuid.NewString(), item.amount, true); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.Reserve(ctx, uuid.New(), item.wallet.ID, item.amount, nil, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(NewService(repo).Handler(strings.Repeat("f", 32), func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{RealmRoles: []string{"platform-admin"}}, nil
	}))
	defer server.Close()
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/overview", nil)
	request.Header.Set("Authorization", "Bearer fixture-token")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != http.StatusOK {
		t.Fatal("valid wallet aggregate did not return a financial overview")
	}
	for _, field := range []string{"posted_microcredits", "reserved_microcredits"} {
		if body[field] != "9223372036854775808" {
			t.Fatal("financial overview truncated an aggregate beyond int64")
		}
	}
	assertBalanced(t, repo)
}
