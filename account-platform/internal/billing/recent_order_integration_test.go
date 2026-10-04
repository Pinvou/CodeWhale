package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPostgresRecentOrderRestoresPreparedOrderAndScopesBothIDs(t *testing.T) {
	p, wallet, requester := paymentFixture(t)
	ctx := context.Background()
	otherWallet, _, err := p.repository.EnsureWallet(ctx, "PERSONAL", uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	p.now = func() time.Time { return now }
	for _, scope := range []struct{ requester, wallet uuid.UUID }{
		{requester, wallet.ID}, {uuid.New(), wallet.ID}, {requester, otherWallet.ID},
	} {
		if _, _, err := p.CreateOrder(ctx, scope.requester, scope.wallet, "CNY_1", "ALIPAY", uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
	}
	p.provider = &recoverableProvider{failFirst: true}
	key := uuid.NewString()
	prepared, _, err := p.CreateOrder(ctx, requester, wallet.ID, "CNY_1", "ALIPAY", key)
	if !errors.Is(err, ErrPaymentProvider) || prepared.Status != "CREATED" {
		t.Fatalf("expected durable preparation after lost provider response: status=%s err=%v", prepared.Status, err)
	}
	var missingURL bool
	if err := p.repository.pool.QueryRow(ctx, `SELECT payment_url IS NULL FROM payment_orders WHERE id=$1`, prepared.ID).Scan(&missingURL); err != nil || !missingURL {
		t.Fatalf("expected original nullable payment_url before activation: %t %v", missingURL, err)
	}

	server := httptest.NewServer(NewService(p.repository).WithPayments(p).Handler("test-only-service-token"))
	defer server.Close()
	lookup := func(userID, walletID uuid.UUID) *PaymentOrder {
		t.Helper()
		query := url.Values{"requester_id": {userID.String()}, "wallet_id": {walletID.String()}}
		request, _ := http.NewRequest(http.MethodGet, server.URL+"/internal/v1/recharge/orders/recent?"+query.Encode(), nil)
		request.Header.Set("Authorization", "Bearer test-only-service-token")
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("recent lookup did not reach the internal production route: %d", response.StatusCode)
		}
		var body struct {
			Order *PaymentOrder `json:"order"`
		}
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body.Order
	}
	latest := lookup(requester, wallet.ID)
	if latest == nil || latest.ID != prepared.ID || latest.Status != "CREATED" || latest.PaymentURL != "" {
		t.Fatal("latest prepared order could not be restored without a payment URL")
	}
	for _, scope := range []struct{ requester, wallet uuid.UUID }{
		{uuid.New(), wallet.ID}, {requester, uuid.New()},
	} {
		if lookup(scope.requester, scope.wallet) != nil {
			t.Fatal("recent order crossed requester or wallet scope")
		}
	}
	recovered, created, err := p.CreateOrder(ctx, requester, wallet.ID, "CNY_1", "ALIPAY", key)
	if err != nil || created || recovered.ID != prepared.ID || recovered.PaymentURL == "" {
		t.Fatalf("prepared order did not resume through its original idempotency key: %v", err)
	}
	latest = lookup(requester, wallet.ID)
	if latest == nil || latest.ID != prepared.ID || latest.Status != "PENDING" || latest.PaymentURL != recovered.PaymentURL {
		t.Fatal("recent lookup lost the recovered order's authoritative payment URL")
	}
}
