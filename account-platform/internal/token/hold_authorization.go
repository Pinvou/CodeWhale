package token

import (
	"context"
	"errors"
	"sync"

	"github.com/google/uuid"
)

var errLocalAuthorizationRecord = errors.New("local preauthorization record is pending")

// A stream's renewal and added authorization share one boundary through the
// Billing acknowledgement and local maximum update. Entries live only while
// an operation holds or waits for their gate.
type usageAuthorizationGates struct {
	mu      sync.Mutex
	entries map[uuid.UUID]*usageAuthorizationGate
}

type usageAuthorizationGate struct {
	busy chan struct{}
	refs int
}

func (g *usageAuthorizationGates) acquire(ctx context.Context, id uuid.UUID) (func(), error) {
	g.mu.Lock()
	if g.entries == nil {
		g.entries = make(map[uuid.UUID]*usageAuthorizationGate)
	}
	entry := g.entries[id]
	if entry == nil {
		entry = &usageAuthorizationGate{busy: make(chan struct{}, 1)}
		g.entries[id] = entry
	}
	entry.refs++
	g.mu.Unlock()
	drop := func() {
		g.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(g.entries, id)
		}
		g.mu.Unlock()
	}
	select {
	case entry.busy <- struct{}{}:
		return func() { <-entry.busy; drop() }, nil
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}

func (s *Service) renewAuthorization(ctx context.Context, usageID, holdID uuid.UUID) error {
	unlock, err := s.usageAuthorization.acquire(ctx, usageID)
	if err != nil {
		return err
	}
	defer unlock()
	client, ok := s.billing.(SnapshotBillingClient)
	if !ok {
		return ErrForbidden
	}
	stored, err := s.repository.GetUsage(ctx, usageID)
	if err != nil {
		return err
	}
	if stored.HoldID == nil || *stored.HoldID != holdID {
		return ErrForbidden
	}
	return client.Extend(ctx, usageID, holdID, stored.WalletID, stored.MaximumMicrocredits, 0)
}

func (s *Service) extendAuthorization(ctx context.Context, usageID, holdID uuid.UUID, maximum, additional int64) error {
	unlock, err := s.usageAuthorization.acquire(ctx, usageID)
	if err != nil {
		return err
	}
	defer unlock()
	client, ok := s.billing.(SnapshotBillingClient)
	if !ok {
		return ErrForbidden
	}
	stored, err := s.repository.GetUsage(ctx, usageID)
	if err != nil {
		return err
	}
	if stored.HoldID == nil || *stored.HoldID != holdID || stored.MaximumMicrocredits != maximum {
		return ErrForbidden
	}
	if err := client.Extend(ctx, usageID, holdID, stored.WalletID, maximum, additional); err != nil {
		return err
	}
	if additional > 0 {
		repository, ok := s.repository.(interface {
			IncreaseMaximum(context.Context, uuid.UUID, int64) error
		})
		if !ok {
			return errLocalAuthorizationRecord
		}
		if err := repository.IncreaseMaximum(ctx, usageID, additional); err != nil {
			return errors.Join(errLocalAuthorizationRecord, err)
		}
	}
	return nil
}
