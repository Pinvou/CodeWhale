package billing

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func paymentWalletAdmissionSnapshot(t *testing.T, ctx context.Context, r *PostgresRepository) map[string]string {
	t.Helper()
	result := make(map[string]string)
	for _, table := range []string{"wallets", "payment_orders", "payment_transactions", "payment_evidence", "idempotency_records", "outbox_events", "journal_entries", "ledger_postings", "entitlement_packages"} {
		var raw string
		if err := r.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(facts) ORDER BY to_jsonb(facts)::text),'[]'::jsonb)::text FROM `+pgx.Identifier{table}.Sanitize()+` facts`).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		result[table] = raw
	}
	return result
}

func TestPaymentPersonalWalletAdmissionStatusRacePostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	owner := uuid.New()
	wallet, _, err := r.EnsureWallet(ctx, "PERSONAL", owner)
	if err != nil {
		t.Fatal(err)
	}
	product := retainedSameKeyProduct(t, r, ctx, owner)
	provider := &coreRecoveryProvider{retainedFinancialProvider: &retainedFinancialProvider{}}
	coordinator, err := NewPaymentCoordinatorWithProvider(r, provider, "test")
	if err != nil {
		t.Fatal(err)
	}
	coordinator.AllowTentativePrices(true)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var holder int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holder); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE wallets SET status='FROZEN' WHERE id=$1`, wallet.ID); err != nil {
		t.Fatal(err)
	}
	callContext, cancel := context.WithCancel(ctx)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, _, err := coordinator.CreateOrder(callContext, owner, wallet.ID, product, "ALIPAY", uuid.NewString())
		finished <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case err := <-finished:
			t.Fatalf("new order did not wait for the wallet status writer: %v", err)
		default:
		}
		var blocked bool
		// Bind the observation to this transaction's actual backend, rather than
		// assuming that a matching SQL string identifies the tested row lock.
		if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, holder).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("new order did not reach the wallet row lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if !errors.Is(err, ErrConflict) {
			t.Fatal("committed wallet suspension did not reject new order", err)
		}
	case <-ctx.Done():
		t.Fatal("new order remained blocked after status commit")
	}
	var orders int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM payment_orders WHERE wallet_id=$1`, wallet.ID).Scan(&orders); err != nil || orders != 0 || provider.creates.Load() != 0 {
		t.Fatal("status race created an order or called provider", orders, provider.creates.Load(), err)
	}
}

func TestPaymentPersonalWalletAdmissionPostgres(t *testing.T) {
	for _, status := range []string{"FROZEN", "CLOSED"} {
		t.Run(status, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			owner := uuid.New()
			wallet, _, err := r.EnsureWallet(ctx, "PERSONAL", owner)
			if err != nil {
				t.Fatal(err)
			}
			product := retainedSameKeyProduct(t, r, ctx, owner)
			provider := &coreRecoveryProvider{retainedFinancialProvider: &retainedFinancialProvider{}}
			coordinator, err := NewPaymentCoordinatorWithProvider(r, provider, "test")
			if err != nil {
				t.Fatal(err)
			}
			coordinator.AllowTentativePrices(true)
			if _, err := r.pool.Exec(ctx, `UPDATE wallets SET status=$2 WHERE id=$1`, wallet.ID, status); err != nil {
				t.Fatal(err)
			}
			before := paymentWalletAdmissionSnapshot(t, ctx, r)
			key := uuid.NewString()
			if _, created, err := coordinator.CreateOrder(ctx, owner, wallet.ID, product, "ALIPAY", key); !errors.Is(err, ErrConflict) || created {
				t.Fatalf("inactive wallet accepted a new order: created=%t err=%v", created, err)
			}
			if provider.creates.Load() != 0 || !reflect.DeepEqual(before, paymentWalletAdmissionSnapshot(t, ctx, r)) {
				t.Fatal("rejected new order called provider or changed durable facts")
			}
			// The same failed key has no receipt and may be used when this controlled
			// wallet is ACTIVE. This positive control reaches the actual provider.
			if _, err := r.pool.Exec(ctx, `UPDATE wallets SET status='ACTIVE' WHERE id=$1`, wallet.ID); err != nil {
				t.Fatal(err)
			}
			order, created, err := coordinator.CreateOrder(ctx, owner, wallet.ID, product, "ALIPAY", key)
			if err != nil || !created || order.ID == uuid.Nil || provider.creates.Load() != 1 {
				t.Fatalf("ACTIVE wallet positive control: created=%t err=%v calls=%d", created, err, provider.creates.Load())
			}
			if _, err := r.pool.Exec(ctx, `UPDATE wallets SET status=$2 WHERE id=$1`, wallet.ID, status); err != nil {
				t.Fatal(err)
			}
			before = paymentWalletAdmissionSnapshot(t, ctx, r)
			replay, created, err := coordinator.CreateOrder(ctx, owner, wallet.ID, product, "ALIPAY", key)
			if err != nil || created || replay.ID != order.ID || replay.PaymentURL != order.PaymentURL || provider.creates.Load() != 1 || !reflect.DeepEqual(before, paymentWalletAdmissionSnapshot(t, ctx, r)) {
				t.Fatal("existing receipt was rejected, redispatched or changed by wallet status", created, err)
			}
			// Reconstruct the existing binding boundary before its response receipt
			// was persisted, as in TestFinanceCreateBindingReplayAfterPreDispatchCrash.
			if _, err := r.pool.Exec(ctx, `DELETE FROM idempotency_records WHERE scope='payment.create' AND idempotency_key=$1`, key); err != nil {
				t.Fatal(err)
			}
			if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET provider_call_status='NOT_STARTED',payment_url=NULL,qr_data=NULL WHERE id=$1`, order.ID); err != nil {
				t.Fatal(err)
			}
			recovered, created, err := coordinator.CreateOrder(ctx, owner, wallet.ID, product, "ALIPAY", key)
			if err != nil || created || recovered.ID != order.ID || recovered.PaymentURL == "" || provider.creates.Load() != 2 {
				t.Fatal("existing order binding could not recover under inactive wallet", created, err, provider.creates.Load())
			}
		})
	}
}
