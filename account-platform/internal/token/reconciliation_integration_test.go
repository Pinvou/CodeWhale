package token

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	billingdomain "github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
)

func TestPostgresReconciliationClaimFencesOldAndExpiredWorkers(t *testing.T) {
	ctx := context.Background()
	repo, usage := settlementFixture(t)
	if err := repo.MarkReview(ctx, usage.ID, "fixture_uncertain", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	repo.now = func() time.Time { return now }
	claim := func() ReconciliationItem {
		t.Helper()
		items, err := repo.ClaimReconciliation(ctx, 50, 45*time.Second)
		if err != nil || len(items) != 1 {
			t.Fatal("expected a single owned usage claim")
		}
		return items[0]
	}
	old := claim()
	now = now.Add(46 * time.Second)
	current := claim()
	if old.Attempts+1 != current.Attempts || !current.LeaseUntil.After(old.LeaseUntil) {
		t.Fatal("claim identity did not advance")
	}
	for _, write := range []func() error{
		func() error { return repo.RescheduleReconciliation(ctx, old, "stale", now.Add(time.Minute)) },
		func() error { return repo.StopReconciliation(ctx, old, "stale") },
	} {
		if write() == nil {
			t.Fatal("old worker changed a newer claim")
		}
		var until time.Time
		var stopped *time.Time
		var attempts int
		if err := repo.pool.QueryRow(ctx, `SELECT reconciliation_lease_until,reconciliation_stopped_at,reconciliation_attempts FROM usage_requests WHERE id=$1`, usage.ID).Scan(&until, &stopped, &attempts); err != nil {
			t.Fatal(err)
		}
		if !until.Equal(current.LeaseUntil) || stopped != nil || attempts != current.Attempts {
			t.Fatal("stale write damaged the current claim")
		}
	}
	if err := repo.RescheduleReconciliation(ctx, current, "retry", now); err != nil {
		t.Fatal(err)
	}
	current = claim()
	now = current.LeaseUntil
	if repo.StopReconciliation(ctx, current, "expired") == nil {
		t.Fatal("expired worker stopped reconciliation")
	}
	current = claim()
	if err := repo.StopReconciliation(ctx, current, "manual_required"); err != nil {
		t.Fatal(err)
	}
	items, err := repo.ClaimReconciliation(ctx, 50, 45*time.Second)
	if err != nil || len(items) != 0 {
		t.Fatal("stopped usage was claimed again")
	}
}

func TestPostgresPendingLegacyUsageNormalizesOnceAfterActualPricingUpgrade(t *testing.T) {
	ctx := context.Background()
	repo, usage := settlementFixture(t)
	pool := testdb.Open(t, "billing")
	price := PriceVersion{ID: uuid.New(), Model: usage.Model, InputPerMillion: 1_000_000, CachedInputPerMillion: 500_000, OutputPerMillion: 2_000_000}
	migratedLegacyBillingPriceFixture(t, pool, price)
	billing, err := billingdomain.NewPostgresRepository(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer billing.Close()
	wallet, _, err := billing.EnsureWallet(ctx, "PERSONAL", usage.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := billing.Credit(ctx, wallet.ID, "owned-legacy-pending", 100, true); err != nil {
		t.Fatal(err)
	}
	hold, err := billing.Reserve(ctx, usage.ID, wallet.ID, 100, &price.ID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.pool.Exec(ctx, `UPDATE usage_requests SET wallet_id=$2,hold_id=$3,price_version_id=$4,request_snapshot=$5 WHERE id=$1`, usage.ID, wallet.ID, hold.ID, price.ID, legacyRequestSnapshotForTest(usage.Model)); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkReview(ctx, usage.ID, "settlement_pending", map[string]any{"usage": Usage{InputTokens: 5, CachedInputTokens: 2, OutputTokens: 2}, "actual_microcredits": int64(8)}); err != nil {
		t.Fatal(err)
	}
	// Exercise the production HTTP client, including the upgraded rich price.
	peer := httptest.NewServer(billingdomain.NewService(billing).Handler(strings.Repeat("p", 32)))
	defer peer.Close()
	client, err := NewHTTPBillingClient(peer.URL, strings.Repeat("p", 32))
	if err != nil {
		t.Fatal(err)
	}
	reconciler := NewUsageReconciler(repo, client, nil, time.Second)
	reconciler.runOnce(ctx)
	reconciler.runOnce(ctx)
	stored, err := repo.GetUsage(ctx, usage.ID)
	if err != nil || stored.Status != "COMMITTED" || stored.ActualMicrocredits == nil || *stored.ActualMicrocredits != 8 {
		t.Fatal("old pending amount was not retained after the actual upgrade")
	}
	balance, err := billing.GetWallet(ctx, wallet.ID)
	if err != nil || balance.PostedMicrocredits != 92 || balance.ReservedMicrocredits != 0 {
		t.Fatal("legacy recovery changed the exact monetary result")
	}
	var input, cached int64
	if err := pool.QueryRow(ctx, `SELECT (usage_snapshot->>'input_tokens')::bigint,(usage_snapshot->>'cached_input_tokens')::bigint FROM usage_holds WHERE id=$1`, hold.ID).Scan(&input, &cached); err != nil || input != 3 || cached != 2 {
		t.Fatal("legacy total input was not converted exactly once")
	}
}
