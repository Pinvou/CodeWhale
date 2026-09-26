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

type archiveBlindRecoveryStoreStub struct {
	items     []ArchiveBlindRecoveryCandidate
	retracted []ArchiveBlindRecoveryCandidate
	err       error
}

func (store *archiveBlindRecoveryStoreStub) ListAutomaticallyMisclassifiedArchiveBlindCases(context.Context, int) ([]ArchiveBlindRecoveryCandidate, error) {
	return store.items, store.err
}

func (store *archiveBlindRecoveryStoreStub) RetractAutomaticallyMisclassifiedArchiveBlindCase(_ context.Context, item ArchiveBlindRecoveryCandidate) (bool, error) {
	store.retracted = append(store.retracted, item)
	return true, nil
}

func TestArchiveBlindRecoveryRequiresFreshSuccessfulProviderQuery(t *testing.T) {
	successID, failedID := uuid.New(), uuid.New()
	store := &archiveBlindRecoveryStoreStub{items: []ArchiveBlindRecoveryCandidate{
		{CaseID: uuid.New(), PaymentOrderID: successID, ProviderID: "provider-a", ProviderVersion: "v1"},
		{CaseID: uuid.New(), PaymentOrderID: failedID, ProviderID: "provider-a", ProviderVersion: "v1"},
	}}
	synchronizer := &paymentSynchronizerStub{
		orders: map[uuid.UUID]PaymentOrder{successID: {ID: successID, Status: "MANUAL_REVIEW"}},
		errors: map[uuid.UUID]error{failedID: errors.New("provider unavailable")},
	}
	worker := NewArchiveBlindRecoveryWorker(store, synchronizer, time.Minute)
	worker.batchSize = len(store.items)
	worker.logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	worker.runOnce(context.Background())

	if len(store.retracted) != 1 || store.retracted[0].PaymentOrderID != successID {
		t.Fatalf("retracted cases = %#v, want only successfully queried order %s", store.retracted, successID)
	}
}

func TestArchiveBlindRecoveryDefaultsToOneProviderProbePerPass(t *testing.T) {
	store := &archiveBlindRecoveryStoreStub{}
	worker := NewArchiveBlindRecoveryWorker(store, &paymentSynchronizerStub{}, time.Minute)

	if worker.batchSize != 1 {
		t.Fatalf("archive blind recovery batch size = %d, want 1", worker.batchSize)
	}
}

func TestArchiveBlindRecoveryStopsWhenCandidateListingFails(t *testing.T) {
	store := &archiveBlindRecoveryStoreStub{err: errors.New("database unavailable")}
	worker := NewArchiveBlindRecoveryWorker(store, &paymentSynchronizerStub{orders: map[uuid.UUID]PaymentOrder{}, errors: map[uuid.UUID]error{}}, time.Minute)
	worker.logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	worker.runOnce(context.Background())

	if len(store.retracted) != 0 {
		t.Fatalf("listing failure retracted cases: %#v", store.retracted)
	}
}
