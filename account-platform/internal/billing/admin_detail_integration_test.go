package billing

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
)

func TestPostgresAdminUnpaidOrderDetailIncludesLoadedEmptyCollections(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	repo := &PostgresRepository{pool: testdb.Open(t, "billing"), now: func() time.Time { return now }}
	user, order := uuid.New(), uuid.New()
	wallet, _, err := repo.EnsureWallet(ctx, "PERSONAL", user)
	if err != nil {
		t.Fatal(err)
	}
	// No provider outcome, transaction, package or payment evidence is fabricated.
	_, err = repo.pool.Exec(ctx, `INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,created_at,updated_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at) VALUES($1,$2,$3,$3,$4,'CNY_1','{}','ALIPAY',100,'CNY',1000000,'CREATED','PENDING',$5,$6,$6,$5,$7,$8)`, order, "detail-"+order.String(), user, wallet.ID, now.Add(time.Minute), now, now.Add(72*time.Hour), now.Add(8*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := repo.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := projectPaymentOrderWithinTransaction(ctx, tx, order, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	detail, err := repo.AdminPaymentOrder(ctx, order)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"transactions", "entitlement_packages", "evidence"} {
		if string(fields[key]) != "[]" {
			t.Errorf("loaded empty %s must be an array; got %q", key, fields[key])
		}
	}
	for summary, key := range map[string]string{"payment_summary": "paid_amount_fen", "refund_summary": "succeeded_amount_fen"} {
		var values map[string]json.RawMessage
		if err := json.Unmarshal(fields[summary], &values); err != nil {
			t.Fatal(err)
		}
		if string(values[key]) != `"0"` {
			t.Errorf("%s.%s money must be an exact decimal string; got %s", summary, key, values[key])
		}
	}
	items, err := repo.AdminPaymentOrders(ctx, AdminPaymentOrderFilter{Query: order.String()}, 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("list projection failed: count=%d err=%v", len(items), err)
	}
	encoded, err = json.Marshal(items[0])
	if err != nil {
		t.Fatal("list projection serialization failed")
	}
	// Unmarshal into a fresh map: an omitted field must not retain detail state.
	fields = nil
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"transactions", "entitlement_packages", "evidence"} {
		if _, exists := fields[key]; exists {
			t.Errorf("unloaded list %s must stay omitted", key)
		}
	}
}

func TestPostgresPaymentSummaryMoneyDefaultsAndHistoricalMigration(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	pool := testdb.Open(t, "billing")
	repo := &PostgresRepository{pool: pool, now: func() time.Time { return now }}
	user, order, transaction, refund := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	wallet, _, err := repo.EnsureWallet(ctx, "PERSONAL", user)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Release()
	loaded, err := migrations.Load(filepath.Join("..", "..", "migrations", "billing"))
	if err != nil {
		t.Fatal(err)
	}
	for {
		var version int64
		if err := connection.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil {
			t.Fatal(err)
		}
		if version <= 23 {
			break
		}
		if err := migrations.DownOne(ctx, connection.Conn(), loaded); err != nil {
			t.Fatal(err)
		}
	}
	// These are the preceding schema's valid historical payment/refund rows;
	// the actual new migration derives read summaries from their exact amounts.
	if _, err := pool.Exec(ctx, `INSERT INTO payment_orders(id,merchant_order_no,requester_id,wallet_id,product_id,product_snapshot,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,paid_at,created_at,updated_at) VALUES($1,$2,$3,$4,'CNY_1','{}','ALIPAY',100,'CNY',1000000,'PAID','CREDITED',$5,$6,$6,$6)`, order, "money-history-"+order.String(), user, wallet.ID, now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO payment_transactions(id,payment_order_id,channel,channel_transaction_id,status,amount_fen,currency,source,verified_at,created_at,updated_at) VALUES($1,$2,'ALIPAY',$3,'SUCCEEDED',100,'CNY','QUERY',$4,$4,$4)`, transaction, order, "history-"+transaction.String(), now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO refunds(id,payment_order_id,requester_id,merchant_refund_no,amount_fen,credit_microcredits,status,idempotency_key,request_hash,reason,created_at,updated_at) VALUES($1,$2,$3,$4,40,400000,'SUCCEEDED',$4,'controlled','historical partial refund',$5,$5)`, refund, order, user, "refund-"+refund.String(), now); err != nil {
		t.Fatal(err)
	}
	for _, migration := range loaded {
		if migration.Version == 24 {
			if err := migrations.Up(ctx, connection.Conn(), []migrations.Migration{migration}); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	check := func(t *testing.T, orderID uuid.UUID, paid, refunded string) {
		t.Helper()
		var payment, refund, chargeback []byte
		if err := pool.QueryRow(ctx, `SELECT payment_summary,refund_summary,chargeback_summary FROM payment_orders WHERE id=$1`, orderID).Scan(&payment, &refund, &chargeback); err != nil {
			t.Fatal(err)
		}
		for _, item := range []struct {
			json          []byte
			key, expected string
		}{{payment, "paid_amount_fen", paid}, {refund, "succeeded_amount_fen", refunded}, {chargeback, "lost_amount_fen", "0"}} {
			var values map[string]json.RawMessage
			if err := json.Unmarshal(item.json, &values); err != nil {
				t.Fatal(err)
			}
			want, _ := json.Marshal(item.expected)
			if string(values[item.key]) != string(want) {
				t.Errorf("%s expected exact money string %s got %s", item.key, want, values[item.key])
			}
		}
	}
	t.Run("historical-backfill", func(t *testing.T) { check(t, order, "100", "40") })
	newOrder := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,created_at,updated_at) VALUES($1,$2,$3,$3,$4,'CNY_1','{}','ALIPAY',100,'CNY',1000000,'CREATED','PENDING',$5,$6,$6)`, newOrder, "money-default-"+newOrder.String(), user, wallet.ID, now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	t.Run("new-defaults", func(t *testing.T) { check(t, newOrder, "0", "0") })
}
