package paymentsim

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A real lock timeout occurs after Begin, the refund-key lock and the receipt
// lookup. It must remain a storage failure, without recording a business result.
func TestPostgresSimulatorRefundStorageFailureRemainsRetryable(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	r, order := simulatorFixture(t, "SUCCESS")
	instant := r.now().Truncate(time.Microsecond)
	r.now = func() time.Time { return instant }
	ctx := context.Background()
	if _, err := r.Complete(ctx, r.paymentToken(order.MerchantOrderNo)); err != nil {
		t.Fatal(err)
	}
	lock, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(ctx)
	if _, err := getOrderByMerchantForUpdate(ctx, lock, order.MerchantOrderNo); err != nil {
		t.Fatal(err)
	}
	config := r.pool.Config()
	config.ConnConfig.RuntimeParams["lock_timeout"] = "100ms"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	blocked := *r
	blocked.pool = pool
	const key = "controlled-storage-retry"
	if _, created, err := blocked.Refund(ctx, order.MerchantOrderNo, key, 40); err == nil || created || errors.Is(err, ErrConflict) {
		t.Errorf("storage failure misclassified: created=%v err=%v", created, err)
	} else {
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != "55P03" {
			t.Fatalf("expected the real order row-lock timeout, got %v", err)
		}
	}
	const token = "controlled-refund-retry-service-token"
	handler := NewService(&blocked).Handler(token)
	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/internal/v1/orders/"+order.MerchantOrderNo+"/refunds", strings.NewReader(`{"merchant_refund_no":"`+key+`","amount_fen":40}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	response := request()
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "INTERNAL_ERROR") || strings.Contains(response.Body.String(), "55P03") || strings.Contains(response.Body.String(), "simulator_orders") {
		t.Fatalf("storage error must be a sanitized HTTP 500: %d %s", response.Code, response.Body.String())
	}
	var refundCount int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM simulator_refunds WHERE merchant_refund_no=$1`, key).Scan(&refundCount); err != nil || refundCount != 0 {
		t.Fatalf("failed transaction persisted refund: count=%d err=%v", refundCount, err)
	}
	stored, err := r.GetByMerchantOrder(ctx, order.MerchantOrderNo)
	if err != nil || stored.Status != "SUCCESS" || stored.RefundedAmountFen != 0 {
		t.Fatalf("storage failure changed payment facts: %+v %v", stored, err)
	}
	if err := lock.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	response = request()
	if response.Code != http.StatusCreated {
		t.Fatalf("same original key must recover after storage becomes available: %d %s", response.Code, response.Body.String())
	}
	var first Refund
	if err := json.Unmarshal(response.Body.Bytes(), &first); err != nil || first.Status != "SUCCESS" || first.AmountFen != 40 {
		t.Fatalf("unexpected successful refund: %+v %v", first, err)
	}
	response = request()
	var replay Refund
	if err := json.Unmarshal(response.Body.Bytes(), &replay); err != nil || response.Code != http.StatusOK || replay.MerchantRefundNo != first.MerchantRefundNo || replay.ChannelRefundID != first.ChannelRefundID || replay.AmountFen != first.AmountFen || replay.Status != first.Status || !replay.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("successful retry must replay exact original refund: %d %+v %v", response.Code, replay, err)
	}
	stored, err = r.GetByMerchantOrder(ctx, order.MerchantOrderNo)
	if err != nil || stored.Status != "PARTIALLY_REFUNDED" || stored.RefundedAmountFen != 40 {
		t.Fatalf("refund applied more than once: %+v %v", stored, err)
	}
	if _, _, err := blocked.Refund(ctx, order.MerchantOrderNo, key, 41); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed idempotency facts must still conflict: %v", err)
	}
	if _, _, err := blocked.Refund(ctx, order.MerchantOrderNo, "controlled-over-refund", 61); !errors.Is(err, ErrConflict) {
		t.Fatalf("over-refund must still conflict: %v", err)
	}
	if _, _, err := blocked.Refund(ctx, "controlled-nonexistent-order", "controlled-missing", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing order must stay not found: %v", err)
	}
}
