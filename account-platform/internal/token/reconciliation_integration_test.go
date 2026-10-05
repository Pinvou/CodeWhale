package token

import (
	"context"
	"testing"
	"time"
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
