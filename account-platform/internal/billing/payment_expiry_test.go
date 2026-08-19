package billing

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
)

type expiredPaymentStoreStub struct {
	items       []ExpiredPaymentCandidate
	rescheduled map[uuid.UUID]time.Time
	errors      map[uuid.UUID]string
}

func (s *expiredPaymentStoreStub) ClaimExpiredPaymentOrders(context.Context, int, time.Duration) ([]ExpiredPaymentCandidate, error) {
	items := s.items
	s.items = nil
	return items, nil
}

func (s *expiredPaymentStoreStub) RescheduleExpiredPaymentOrder(_ context.Context, orderID uuid.UUID, lastError string, nextAt time.Time) error {
	s.rescheduled[orderID] = nextAt
	s.errors[orderID] = lastError
	return nil
}

type paymentSynchronizerStub struct {
	orders map[uuid.UUID]PaymentOrder
	errors map[uuid.UUID]error
}

func (s *paymentSynchronizerStub) SyncOrder(_ context.Context, orderID uuid.UUID) (PaymentOrder, error) {
	return s.orders[orderID], s.errors[orderID]
}

func TestExpiredPaymentReconcilerReschedulesFailuresAndNonTerminalProviderStates(t *testing.T) {
	closedID, pendingID, failedID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	store := &expiredPaymentStoreStub{
		items: []ExpiredPaymentCandidate{
			{OrderID: closedID, Attempts: 1},
			{OrderID: pendingID, Attempts: 2},
			{OrderID: failedID, Attempts: 3},
		},
		rescheduled: map[uuid.UUID]time.Time{},
		errors:      map[uuid.UUID]string{},
	}
	synchronizer := &paymentSynchronizerStub{
		orders: map[uuid.UUID]PaymentOrder{
			closedID:  {ID: closedID, Status: "CLOSED"},
			pendingID: {ID: pendingID, Status: "PENDING"},
		},
		errors: map[uuid.UUID]error{failedID: errors.New("provider unavailable")},
	}
	now := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	reconciler := NewExpiredPaymentReconciler(store, synchronizer, 15*time.Second)
	reconciler.now = func() time.Time { return now }
	reconciler.logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	reconciler.runOnce(context.Background())

	if _, exists := store.rescheduled[closedID]; exists {
		t.Fatal("closed order must not be rescheduled")
	}
	if got := store.rescheduled[pendingID]; !got.Equal(now.Add(30 * time.Second)) {
		t.Fatalf("pending order retry = %v, want %v", got, now.Add(30*time.Second))
	}
	if got := store.rescheduled[failedID]; !got.Equal(now.Add(time.Minute)) {
		t.Fatalf("failed order retry = %v, want %v", got, now.Add(time.Minute))
	}
	if store.errors[pendingID] != "payment order synchronization failed" || store.errors[failedID] != "payment order synchronization failed" {
		t.Fatalf("unexpected retry errors: %#v", store.errors)
	}
}

func TestPaymentReconciliationErrorsDoNotPersistProviderDetails(t *testing.T) {
	providerError := errors.Join(ErrPaymentProvider, errors.New("upstream body with sensitive details"))
	if got := safePaymentReconciliationError(providerError); got != "payment provider unavailable" {
		t.Fatalf("provider error = %q", got)
	}
	if got := safePaymentReconciliationError(ErrConflict); got != "payment provider state conflict" {
		t.Fatalf("conflict error = %q", got)
	}
}

func TestPaymentReconciliationBackoffIsBounded(t *testing.T) {
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: 0, want: 15 * time.Second},
		{attempt: 1, want: 15 * time.Second},
		{attempt: 2, want: 30 * time.Second},
		{attempt: 3, want: time.Minute},
		{attempt: 100, want: 5 * time.Minute},
	}
	for _, test := range tests {
		if got := paymentReconciliationBackoff(test.attempt); got != test.want {
			t.Fatalf("attempt %d: got %v, want %v", test.attempt, got, test.want)
		}
	}
}

func TestPaymentReconciliationErrorIsTrimmedAndBounded(t *testing.T) {
	if got := truncatePaymentReconciliationError("  provider unavailable  "); got != "provider unavailable" {
		t.Fatalf("unexpected trimmed error %q", got)
	}
	long := make([]byte, 600)
	for i := range long {
		long[i] = 'x'
	}
	if got := truncatePaymentReconciliationError(string(long)); len(got) != 500 {
		t.Fatalf("error length = %d, want 500", len(got))
	}
}
