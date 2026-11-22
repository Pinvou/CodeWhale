package token

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/google/uuid"
)

// Only schedules the original reservation between a real read and confirmation.
// All persisted usage, reservation and confirmation operations remain real.
type cancelReadBoundaryRepository struct {
	*PostgresRepository
	afterRead func(UsageRequest)
}

func (r *cancelReadBoundaryRepository) GetUsage(ctx context.Context, id uuid.UUID) (UsageRequest, error) {
	u, err := r.PostgresRepository.GetUsage(ctx, id)
	if err == nil && r.afterRead != nil {
		f := r.afterRead
		r.afterRead = nil
		f(u)
	}
	return u, err
}

func TestCancellationReceivedReadRechecksReservationPostgres(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	r, ctx := retainedManagementTokenRepository(t)
	funds, _, bp := retainedManagementBillingRepository(t)
	server := httptest.NewServer(billing.NewService(funds).Handler("controlled-cancel-boundary"))
	defer server.Close()
	client, err := NewHTTPBillingClient(server.URL, "controlled-cancel-boundary")
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range []struct {
		name                           string
		reserve, acknowledged, wrongID bool
	}{
		{name: "not-started"},
		{name: "reserve-ack-pending", reserve: true},
		{name: "reserve-ack-attached", reserve: true, acknowledged: true},
		{name: "wrong-hold-then-original", reserve: true, acknowledged: true, wrongID: true},
	} {
		t.Run(sample.name, func(t *testing.T) {
			u, envelope := jR6PreparedSeed(t, r, funds, bp, client, ctx)
			before, err := funds.GetWallet(ctx, u.WalletID)
			if err != nil || before.ReservedMicrocredits != 0 {
				t.Fatal("unreserved original wallet", err)
			}
			if _, pre, err := r.RequestCancellation(ctx, u.ID, u.UserID, digestString([]byte("cancel-boundary-"+u.ID.String())), cancellationFingerprint(u.ID), "controlled-v1"); err != nil || !pre {
				t.Fatal("real user cancellation", pre, err)
			}
			source := &Service{repository: r, billing: client, cancellationBilling: client, cancellationStore: r}
			var hold BillingHold
			boundary := &cancelReadBoundaryRepository{PostgresRepository: r}
			boundary.afterRead = func(snapshot UsageRequest) {
				if snapshot.ExecutionStatus != "RECEIVED" || snapshot.CanonicalVersion != 1 || snapshot.HoldID != nil {
					t.Fatal("unexpected original read", snapshot.ExecutionStatus, snapshot.CanonicalVersion)
				}
				// The original caller resumes at proxy.go's existing reservation boundary.
				if err := r.MarkHoldReserving(ctx, u.ID); err != nil {
					t.Fatal("real reservation transition", err)
				}
				var err error
				hold, err = source.ensureUsageHold(ctx, u, envelope)
				if err != nil {
					t.Fatal("real original reserve", err)
				}
				if sample.acknowledged {
					if err := r.CompleteHoldAndDispatch(ctx, u.ID, hold.ID, envelope.Price.ID); !errors.Is(err, ErrCancellationRequested) {
						t.Fatal("original promotion cancellation fence", err)
					}
				}
				if sample.wrongID {
					if _, err := r.ConfirmPreDispatchCancellation(ctx, u.ID, uuid.New()); !errors.Is(err, ErrForbidden) {
						t.Fatal("unrelated hold authorized confirmation", err)
					}
					current, err := r.GetUsage(ctx, u.ID)
					if err != nil || current.ExecutionStatus != "HOLD_RESERVED" || current.SettlementStatus != "HOLD_ACTIVE" {
						t.Fatal("unrelated hold changed original state", current.ExecutionStatus, err)
					}
				}
			}
			if !sample.reserve {
				boundary.afterRead = nil
			}
			cancellation := &Service{repository: r, billing: client, cancellationBilling: client, cancellationStore: boundary}
			result, err := cancellation.finishPreDispatchCancellation(ctx, u.ID)
			if err != nil {
				t.Fatal("original cancel operation", err)
			}
			wallet, err := funds.GetWallet(ctx, u.WalletID)
			if err != nil {
				t.Fatal(err)
			}
			if result.SettlementStatus != "RELEASED" || result.CancelStatus != "LOCAL_CONFIRMED" || wallet.ReservedMicrocredits != 0 || wallet.PostedMicrocredits != before.PostedMicrocredits {
				t.Fatalf("confirmation must match actual Billing funds: result=%s/%s reserved=%d", result.SettlementStatus, result.CancelStatus, wallet.ReservedMicrocredits)
			}
			var count int
			if err := bp.QueryRow(ctx, `SELECT count(*) FROM usage_holds WHERE usage_request_id=$1`, u.ID).Scan(&count); err != nil || sample.reserve && count != 1 || !sample.reserve && count != 0 {
				t.Fatal("cancel changed original reservation identity/count", count, err)
			}
			if sample.reserve {
				current, err := r.GetUsage(ctx, u.ID)
				if err != nil || current.HoldID == nil || *current.HoldID != hold.ID || current.PriceVersionID == nil || *current.PriceVersionID != envelope.Price.ID {
					t.Fatal("confirmation lost original hold/price", err)
				}
				if err := r.CompleteHoldAndDispatch(ctx, u.ID, hold.ID, envelope.Price.ID); !errors.Is(err, ErrForbidden) {
					t.Fatal("late original acknowledgement resumed cancelled dispatch", err)
				}
			}
			replayed, pre, err := r.RequestCancellation(ctx, u.ID, u.UserID, digestString([]byte("cancel-boundary-"+u.ID.String())), cancellationFingerprint(u.ID), "controlled-v1")
			if err != nil || pre || replayed.CancelStatus != "LOCAL_CONFIRMED" || replayed.SettlementStatus != "RELEASED" {
				t.Fatal("original cancellation replay changed terminal facts", replayed, pre, err)
			}
		})
	}
}
