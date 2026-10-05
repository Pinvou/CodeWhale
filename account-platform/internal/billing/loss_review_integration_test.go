package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// This tests only a pre-existing explicit terminal-release proof; no expiry
// worker or automatic waiver is introduced to manufacture that proof.
func TestPostgresUsageLossReviewPreservesFirstClaimFacts(t *testing.T) {
	r, wallet := billingFixture(t)
	ctx := context.Background()
	if _, err := r.Credit(ctx, wallet.ID, "loss-fixture", 100, true); err != nil {
		t.Fatal(err)
	}
	usageID := uuid.New()
	hold, err := r.Reserve(ctx, usageID, wallet.ID, 50, nil, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Release(ctx, usageID, hold.ID, "ENTITLEMENT_EXPIRED_UNCERTAIN"); err != nil {
		t.Fatal(err)
	}
	facts := map[string]any{"input_tokens": 2, "output_tokens": 3}
	for range 2 {
		if _, err := r.Commit(ctx, usageID, hold.ID, 20, facts); !errors.Is(err, ErrUsagePlatformLossReview) {
			t.Fatalf("same claim not convergent: %v", err)
		}
	}
	for _, changed := range []struct {
		actual int64
		facts  map[string]any
	}{{21, facts}, {20, map[string]any{"input_tokens": 2, "output_tokens": 4}}} {
		if _, err := r.Commit(ctx, usageID, hold.ID, changed.actual, changed.facts); !errors.Is(err, ErrConflict) {
			t.Fatalf("changed immutable loss claim accepted: %v", err)
		}
	}
	var count int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_loss_reviews WHERE hold_id=$1 AND claimed_actual_microcredits=20 AND usage_snapshot='{"input_tokens":2,"output_tokens":3}'::jsonb`, hold.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("first loss evidence changed: %d %v", count, err)
	}
	current, err := r.GetWallet(ctx, wallet.ID)
	if err != nil || current.PostedMicrocredits != 100 || current.ReservedMicrocredits != 0 {
		t.Fatalf("loss claim charged wallet: %+v %v", current, err)
	}
	assertBalanced(t, r)
}
