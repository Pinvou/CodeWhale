package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPostgresExpiredPaymentLeaseRejectsStaleAndExpiredWriters(t *testing.T) {
	p, wallet, user := paymentFixture(t)
	ctx := context.Background()
	order, _, err := p.CreateOrder(ctx, user, wallet.ID, "CNY_1", "ALIPAY", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	p.repository.now = func() time.Time { return now }
	if _, err := p.repository.pool.Exec(ctx, `UPDATE payment_orders SET expires_at=$2 WHERE id=$1`, order.ID, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	claim := func() ExpiredPaymentCandidate {
		t.Helper()
		items, err := p.repository.ClaimExpiredPaymentOrders(ctx, 1, 5*time.Minute)
		if err != nil || len(items) != 1 {
			t.Fatal("expected one expired payment claim")
		}
		return items[0]
	}
	old := claim()
	if items, err := p.repository.ClaimExpiredPaymentOrders(ctx, 1, 5*time.Minute); err != nil || len(items) != 0 {
		t.Fatal("live claim was not exclusive")
	}
	now = now.Add(6 * time.Minute)
	current := claim()
	if current.Attempts != old.Attempts+1 || !current.LeaseUntil.After(old.LeaseUntil) {
		t.Fatal("new lease identity did not advance")
	}
	if err := p.repository.RescheduleExpiredPaymentOrder(ctx, old, "old", now.Add(time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatal("stale writer was accepted")
	}
	var lease time.Time
	if err := p.repository.pool.QueryRow(ctx, `SELECT reconciliation_lease_until FROM payment_orders WHERE id=$1`, order.ID).Scan(&lease); err != nil || !lease.Equal(current.LeaseUntil) {
		t.Fatal("stale writer erased the new lease")
	}
	if err := p.repository.RescheduleExpiredPaymentOrder(ctx, current, "payment provider unavailable", now); err != nil {
		t.Fatal(err)
	}
	expired := claim()
	now = expired.LeaseUntil
	if err := p.repository.RescheduleExpiredPaymentOrder(ctx, expired, "late", now); !errors.Is(err, ErrConflict) {
		t.Fatal("expired writer was accepted")
	}
	newest := claim()
	if _, err := p.applyProviderTerminalStatus(ctx, order, "CLOSED"); err != nil {
		t.Fatal(err)
	}
	if err := p.repository.RescheduleExpiredPaymentOrder(ctx, newest, "late", now); !errors.Is(err, ErrConflict) {
		t.Fatal("terminal order was reopened")
	}
	var status string
	var attempts int
	if err := p.repository.pool.QueryRow(ctx, `SELECT status,reconciliation_attempts FROM payment_orders WHERE id=$1`, order.ID).Scan(&status, &attempts); err != nil || status != "CLOSED" || attempts != 4 {
		t.Fatal("terminal facts or claim count changed")
	}
}

type deadlinePaymentSynchronizer struct {
	calls   int
	bounded bool
}

func (s *deadlinePaymentSynchronizer) SyncOrder(ctx context.Context, id uuid.UUID) (PaymentOrder, error) {
	s.calls++
	deadline, ok := ctx.Deadline()
	s.bounded = ok && time.Until(deadline) <= time.Second
	return PaymentOrder{ID: id, Status: "PENDING"}, nil
}

func TestExpiredPaymentReconcilerSkipsExpiredBatchItemsAndBoundsActiveWork(t *testing.T) {
	now := time.Now()
	expired, active := uuid.New(), uuid.New()
	store := &expiredPaymentStoreStub{items: []ExpiredPaymentCandidate{{OrderID: expired, Attempts: 1, LeaseUntil: now}, {OrderID: active, Attempts: 1, LeaseUntil: now.Add(time.Second), LateReviewBoundaryAt: now.Add(time.Hour)}}, rescheduled: map[uuid.UUID]time.Time{}, errors: map[uuid.UUID]string{}, escalated: map[uuid.UUID]string{}}
	syncer := &deadlinePaymentSynchronizer{}
	reconciler := NewExpiredPaymentReconciler(store, syncer, time.Second)
	reconciler.now = func() time.Time { return now }
	reconciler.runOnce(context.Background())
	if syncer.calls != 1 || !syncer.bounded || len(store.rescheduled) != 1 {
		t.Fatal("expired batch item ran or active work outlived its lease")
	}
	if _, found := store.rescheduled[active]; !found {
		t.Fatal("active pending order was not rescheduled")
	}
}
