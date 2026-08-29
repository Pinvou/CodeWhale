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
	escalated   map[uuid.UUID]string
}

func (s *expiredPaymentStoreStub) EscalateLatePaymentReview(_ context.Context, orderID uuid.UUID, queryResult string) (LatePaymentReviewCase, error) {
	s.escalated[orderID] = queryResult
	return LatePaymentReviewCase{PaymentOrderID: orderID}, nil
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
	now := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	store := &expiredPaymentStoreStub{
		items: []ExpiredPaymentCandidate{
			{OrderID: closedID, Attempts: 1, LateReviewBoundaryAt: now.Add(time.Hour)},
			{OrderID: pendingID, Attempts: 2, LateReviewBoundaryAt: now.Add(time.Hour)},
			{OrderID: failedID, Attempts: 3, LateReviewBoundaryAt: now.Add(time.Hour)},
		},
		rescheduled: map[uuid.UUID]time.Time{},
		errors:      map[uuid.UUID]string{},
		escalated:   map[uuid.UUID]string{},
	}
	synchronizer := &paymentSynchronizerStub{
		orders: map[uuid.UUID]PaymentOrder{
			closedID:  {ID: closedID, Status: "CLOSED"},
			pendingID: {ID: pendingID, Status: "PENDING"},
		},
		errors: map[uuid.UUID]error{failedID: errors.New("provider unavailable")},
	}
	reconciler := NewExpiredPaymentReconciler(store, synchronizer, 15*time.Second)
	reconciler.now = func() time.Time { return now }
	reconciler.logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	reconciler.runOnce(context.Background())

	if _, exists := store.rescheduled[closedID]; exists {
		t.Fatal("closed order must not be rescheduled")
	}
	if got := store.rescheduled[pendingID]; !got.Equal(now.Add(15 * time.Second)) {
		t.Fatalf("pending order retry = %v, want %v", got, now.Add(15*time.Second))
	}
	if got := store.rescheduled[failedID]; !got.Equal(now.Add(time.Minute)) {
		t.Fatalf("failed order retry = %v, want %v", got, now.Add(time.Minute))
	}
	if store.errors[pendingID] != "" || store.errors[failedID] != "payment order synchronization failed" {
		t.Fatalf("unexpected retry errors: %#v", store.errors)
	}
}

func TestExpiredPaymentReconcilerEscalatesExactlyAfterBoundaryQuery(t *testing.T) {
	orderID := uuid.Must(uuid.NewV7())
	now := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	store := &expiredPaymentStoreStub{items: []ExpiredPaymentCandidate{{OrderID: orderID, Attempts: 9, LateReviewBoundaryAt: now}}, rescheduled: map[uuid.UUID]time.Time{}, errors: map[uuid.UUID]string{}, escalated: map[uuid.UUID]string{}}
	synchronizer := &paymentSynchronizerStub{orders: map[uuid.UUID]PaymentOrder{orderID: {ID: orderID, Status: "PENDING"}}, errors: map[uuid.UUID]error{}}
	reconciler := NewExpiredPaymentReconciler(store, synchronizer, 15*time.Second)
	reconciler.now = func() time.Time { return now }
	reconciler.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	reconciler.runOnce(context.Background())
	if store.escalated[orderID] != "PENDING" {
		t.Fatalf("boundary result = %q", store.escalated[orderID])
	}
	if _, exists := store.rescheduled[orderID]; exists {
		t.Fatal("boundary order must stop high-frequency polling")
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

func TestExpiredPendingPaymentRequiresLocalCloseAtDeadline(t *testing.T) {
	now := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	base := PaymentOrder{Status: "PENDING", CloseStatus: "OPEN", ExpiresAt: now}
	provider := ProviderOrder{Status: "PENDING"}
	if !shouldCloseExpiredPaymentOrder(base, provider, now) {
		t.Fatal("pending order must be locally closed when its payment deadline is reached")
	}
	base.CloseStatus = "LOCALLY_CLOSED_EXTERNAL_PENDING"
	if !shouldCloseExpiredPaymentOrder(base, provider, now) {
		t.Fatal("locally closed order must remain eligible for provider close convergence")
	}
	base.CloseStatus = "OPEN"
	base.ExpiresAt = now.Add(time.Second)
	if shouldCloseExpiredPaymentOrder(base, provider, now) {
		t.Fatal("order must not close before its payment deadline")
	}
	base.ExpiresAt = now
	provider.Status = "SUCCESS"
	if shouldCloseExpiredPaymentOrder(base, provider, now) {
		t.Fatal("confirmed payment must take precedence over expiry close")
	}
}

func TestProviderPaymentDeadlinePrefersFinalPayableUntil(t *testing.T) {
	payableUntil := time.Date(2026, 8, 29, 10, 15, 0, 0, time.UTC)
	instructionExpires := payableUntil.Add(-time.Minute)
	got := providerPaymentDeadline(ProviderOrder{ProviderPayableUntil: &payableUntil, PaymentInstructionExpiresAt: &instructionExpires})
	if got == nil || !got.Equal(payableUntil) {
		t.Fatalf("provider deadline = %v, want %v", got, payableUntil)
	}
	got = providerPaymentDeadline(ProviderOrder{PaymentInstructionExpiresAt: &instructionExpires})
	if got == nil || !got.Equal(instructionExpires) {
		t.Fatalf("instruction deadline fallback = %v, want %v", got, instructionExpires)
	}
}
