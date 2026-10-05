package account

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"testing"
	"time"
)

type cleanupProfileStore struct {
	profileStoreFake
	stage   string
	bounded bool
}

func (f *cleanupProfileStore) CommitProfileChange(ctx context.Context, id uuid.UUID) error {
	if f.stage == "account" {
		return errors.New("controlled account failure")
	}
	return f.profileStoreFake.CommitProfileChange(ctx, id)
}
func (f *cleanupProfileStore) ScheduleProfileChangeRetry(ctx context.Context, _ uuid.UUID, _ string) error {
	deadline, ok := ctx.Deadline()
	f.bounded = ok && ctx.Err() == nil && time.Until(deadline) > 0 && time.Until(deadline) <= 2*time.Second
	return nil
}

type cleanupProfileIdentity struct {
	profileIdentityFake
	stage string
}

func (f *cleanupProfileIdentity) UpdateUsername(ctx context.Context, id string, op uuid.UUID, name string) error {
	if f.stage == "identity" {
		return errors.New("controlled identity failure")
	}
	return f.profileIdentityFake.UpdateUsername(ctx, id, op, name)
}
func (f *cleanupProfileIdentity) RevokeSessions(ctx context.Context, id string, operationID uuid.UUID, firstAttempt bool) error {
	if f.stage == "sessions" {
		return errors.New("controlled revoke failure")
	}
	return f.profileIdentityFake.RevokeSessions(ctx, id, operationID, firstAttempt)
}
func TestProfileRecoveryCleanupIsDetachedAndBounded(t *testing.T) {
	for _, stage := range []string{"identity", "account", "sessions"} {
		t.Run(stage, func(t *testing.T) {
			store := &cleanupProfileStore{stage: stage}
			store.change = ProfileChange{ID: uuid.New(), Field: "USERNAME", Status: "VERIFIED"}
			remote := &cleanupProfileIdentity{stage: stage}
			service := &ProfileChangeService{store: store, identities: remote, now: time.Now}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err := service.resume(ctx, store.change)
			if err == nil || !store.bounded {
				t.Fatal("failed profile operation did not use a bounded detached recovery write")
			}
		})
	}
}
