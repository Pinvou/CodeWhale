package billing

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"sync/atomic"
	"testing"
	"time"
)

// Atomic test clock advances only after pg_stat_activity proves the production
// caller is waiting on this fixture's wallet row. Elapsed sleep proves nothing.
func personalHoldClockFixture(t *testing.T) (*PostgresRepository, context.Context, Wallet, *atomic.Int64, time.Time) {
	t.Helper()
	r, ctx := retainedFinanceRepository(t)
	base := r.now().Truncate(time.Microsecond)
	clock := &atomic.Int64{}
	clock.Store(base.UnixNano())
	r.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	w, _, err := r.EnsureWallet(ctx, "PERSONAL", uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	w, err = r.Credit(ctx, w.ID, "clock-fixture-"+uuid.NewString(), 10000, false)
	if err != nil {
		t.Fatal(err)
	}
	return r, ctx, w, clock, base
}
func waitForPersonalWalletWaiters(t *testing.T, ctx context.Context, r *PostgresRepository, wanted int) {
	t.Helper()
	end := time.Now().Add(3 * time.Second)
	for time.Now().Before(end) {
		var count int
		err := r.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%FROM wallets WHERE id=$1%'`).Scan(&count)
		if err != nil {
			t.Fatal("owned wait-state query failed", err)
		}
		if count >= wanted {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("production caller did not reach exact controlled wallet lock wait")
}
func blockPersonalWallet(t *testing.T, ctx context.Context, r *PostgresRepository, id uuid.UUID) pgx.Tx {
	t.Helper()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback(context.Background()) })
	if _, err = getWallet(ctx, tx, id, true); err != nil {
		t.Fatal(err)
	}
	return tx
}
func TestPersonalUsageHoldClockBoundaries(t *testing.T) {
	t.Run("reserve-request-deadline-crosses-wallet-lock", func(t *testing.T) {
		r, ctx, w, clock, base := personalHoldClockFixture(t)
		tx := blockPersonalWallet(t, ctx, r, w.ID)
		id := uuid.New()
		done := make(chan error, 1)
		go func() { _, err := r.Reserve(ctx, id, w.ID, 100, nil, base.Add(time.Minute)); done <- err }()
		waitForPersonalWalletWaiters(t, ctx, r, 1)
		clock.Store(base.Add(2 * time.Minute).UnixNano())
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		var err error
		select {
		case err = <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("reserve did not leave controlled lock")
		}
		if !errors.Is(err, ErrConflict) {
			t.Error("expired request was accepted after wallet lock", err)
		}
		var reserved, count int64
		if err = r.pool.QueryRow(ctx, `SELECT reserved_microcredits,(SELECT count(*) FROM usage_holds WHERE usage_request_id=$2) FROM wallets WHERE id=$1`, w.ID, id).Scan(&reserved, &count); err != nil || reserved != 0 || count != 0 {
			t.Error("expired request created money reservation", reserved, count, err)
		}
	})
	t.Run("reserve-component-expiry-crosses-wallet-lock", func(t *testing.T) {
		r, ctx, w, clock, base := personalHoldClockFixture(t)
		tx := blockPersonalWallet(t, ctx, r, w.ID)
		id := uuid.New()
		done := make(chan error, 1)
		go func() { _, err := r.Reserve(ctx, id, w.ID, 100, nil, base.AddDate(0, 0, 31)); done <- err }()
		waitForPersonalWalletWaiters(t, ctx, r, 1)
		clock.Store(base.AddDate(0, 0, 30).Add(time.Minute).UnixNano())
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		var err error
		select {
		case err = <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("reserve did not leave controlled lock")
		}
		if !errors.Is(err, ErrInsufficientBalance) {
			t.Error("expired component was reserved with stale eligibility clock", err)
		}
		var reserved, count int64
		if err = r.pool.QueryRow(ctx, `SELECT reserved_microcredits,(SELECT count(*) FROM usage_holds WHERE usage_request_id=$2) FROM wallets WHERE id=$1`, w.ID, id).Scan(&reserved, &count); err != nil || reserved != 0 || count != 0 {
			t.Error("expired component created money reservation", reserved, count, err)
		}
	})
	t.Run("extend-request-deadline-crosses-wallet-lock", func(t *testing.T) {
		r, ctx, w, clock, base := personalHoldClockFixture(t)
		id := uuid.New()
		h, err := r.Reserve(ctx, id, w.ID, 100, nil, base.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		tx := blockPersonalWallet(t, ctx, r, w.ID)
		done := make(chan error, 1)
		go func() { _, err := r.ExtendHold(ctx, id, h.ID, base.Add(time.Minute), 0); done <- err }()
		waitForPersonalWalletWaiters(t, ctx, r, 1)
		clock.Store(base.Add(2 * time.Minute).UnixNano())
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err = <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("extension did not leave controlled lock")
		}
		if !errors.Is(err, ErrConflict) {
			t.Error("expired extension request was accepted after lock", err)
		}
		got, err := getHoldByUsage(ctx, r.pool, id, false)
		if err != nil || got.Status != "ACTIVE" || !got.ExpiresAt.Equal(h.ExpiresAt) || got.MaxMicrocredits != 100 {
			t.Error("invalid extension changed original active hold", err)
		}
	})
	t.Run("expired-candidate-does-not-release-successful-renewal", func(t *testing.T) {
		r, ctx, w, clock, base := personalHoldClockFixture(t)
		id := uuid.New()
		h, err := r.Reserve(ctx, id, w.ID, 100, nil, base.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		clock.Store(base.Add(2 * time.Minute).UnixNano())
		tx := blockPersonalWallet(t, ctx, r, w.ID)
		extension := make(chan error, 1)
		go func() { _, err := r.ExtendHold(ctx, id, h.ID, base.Add(time.Hour), 0); extension <- err }()
		waitForPersonalWalletWaiters(t, ctx, r, 1)
		type expiredResult struct {
			count int
			err   error
		}
		expiry := make(chan expiredResult, 1)
		go func() { count, err := r.ExpireHolds(ctx, 100); expiry <- expiredResult{count, err} }()
		waitForPersonalWalletWaiters(t, ctx, r, 2)
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err = <-extension:
			if err != nil {
				t.Fatal("still-active original hold failed genuine renewal", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("renewal did not leave controlled lock")
		}
		select {
		case result := <-expiry:
			if result.err != nil || result.count != 0 {
				t.Error("stale expiry candidate released renewed hold", result.count, result.err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("expiry did not leave controlled lock")
		}
		got, err := getHoldByUsage(ctx, r.pool, id, false)
		if err != nil || got.Status != "ACTIVE" || !got.ExpiresAt.Equal(base.Add(time.Hour)) {
			t.Error("successful renewal lost original active hold", err)
		}
		var reserved int64
		if err = r.pool.QueryRow(ctx, `SELECT reserved_microcredits FROM wallets WHERE id=$1`, w.ID).Scan(&reserved); err != nil || reserved != 100 {
			t.Error("renewed usage lost wallet reservation", err)
		}
	})
}
