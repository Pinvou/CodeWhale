package token

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

type sequentialReconciliationFake struct {
	ownedReconciliationStoreFake
	t     *testing.T
	items []ReconciliationItem
	calls int
	prior uuid.UUID
}

func (s *sequentialReconciliationFake) ClaimReconciliation(context.Context, int, time.Duration) ([]ReconciliationItem, error) {
	s.t.Fatal("sequential worker used the bulk claim path")
	return nil, nil
}

func (s *sequentialReconciliationFake) ClaimReconciliationExcluding(_ context.Context, limit int, lease time.Duration, excluded []uuid.UUID) ([]ReconciliationItem, error) {
	s.t.Helper()
	if limit != 1 || lease != 45*time.Second || s.prior != uuid.Nil && s.stoppedID != s.prior {
		s.t.Fatal("next lease taken before prior item finished, or lease policy changed")
	}
	s.calls++
	for _, item := range s.items {
		found := false
		for _, id := range excluded {
			found = found || id == item.ID
		}
		if !found {
			s.prior = item.ID
			return []ReconciliationItem{item}, nil
		}
	}
	return nil, nil
}

func TestUsageReconcilerClaimsOnlyWhenReadyAndExcludesSeen(t *testing.T) {
	store := &sequentialReconciliationFake{t: t}
	for range 2 {
		store.items = append(store.items, ReconciliationItem{UsageRequest: UsageRequest{ID: uuid.New(), Status: "REVIEW", StartedAt: time.Now()}, Attempts: 7})
	}
	worker := NewUsageReconciler(store, &reconciliationBillingFake{}, nil, time.Second)
	worker.runOnce(context.Background())
	if store.calls != 3 || store.stoppedID != store.items[1].ID || store.attempt != 7 {
		t.Fatal("immediately-due first item repeated, next item skipped or claim attempt lost")
	}
}
