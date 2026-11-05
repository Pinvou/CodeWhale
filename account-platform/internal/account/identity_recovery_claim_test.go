package account

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"testing"
	"time"
)

func TestIdentityRecoveryClaimContextBoundaries(t *testing.T) {
	id := uuid.New()
	until := time.Now().Add(time.Minute)
	for _, bad := range []struct {
		id         uuid.UUID
		generation int64
		until      *time.Time
	}{{uuid.Nil, 1, &until}, {id, 0, &until}, {id, 1, nil}} {
		if _, _, err := withIdentityRecoveryClaim(context.Background(), "registration", bad.id, bad.generation, bad.until); !errors.Is(err, ErrIdentityRecoveryClaimLost) {
			t.Fatal("missing claim accepted", err)
		}
	}
	ctx, cancel, err := withIdentityRecoveryClaim(context.Background(), "registration", id, 7, &until)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	detached := context.WithoutCancel(ctx)
	claim, ok := detached.Value(identityRecoveryClaimKey{}).(identityRecoveryClaim)
	if !ok || claim.id != id || claim.generation != 7 || !claim.until.Equal(until) || detached.Err() != nil {
		t.Fatal("detached cleanup lost durable claim facts")
	}
	expired := time.Now().Add(-time.Second)
	if _, _, err := withIdentityRecoveryClaim(context.Background(), "registration", id, 1, &expired); !errors.Is(err, ErrIdentityRecoveryClaimLost) {
		t.Fatal("expired batch item can dispatch", err)
	}
}

type missingRegistrationRecoveryClaim struct{}

func (missingRegistrationRecoveryClaim) LeaseRegistrationRecoveries(context.Context, int, time.Duration) ([]RegistrationOperation, error) {
	return []RegistrationOperation{{ID: uuid.New()}}, nil
}

type missingProfileRecoveryClaim struct {
	profileStoreFake
	resetClaims int
}

func (*missingProfileRecoveryClaim) ClaimProfileChange(context.Context) (*ProfileChange, error) {
	return &ProfileChange{ID: uuid.New()}, nil
}
func (s *missingProfileRecoveryClaim) ClaimPasswordReset(context.Context) (*PasswordResetOperation, error) {
	s.resetClaims++
	return nil, nil
}
func TestIdentityRecoveryWorkersRequireDurableClaims(t *testing.T) {
	registration := NewRegistrationRecoveryWorker(missingRegistrationRecoveryClaim{}, &MobileAuthService{}, time.Second)
	if err := registration.RunOnce(context.Background()); !errors.Is(err, ErrIdentityRecoveryClaimLost) {
		t.Fatal("registration worker dispatched without a durable claim", err)
	}
	store := &missingProfileRecoveryClaim{}
	profile := NewProfileChangeWorker(&ProfileChangeService{store: store}, time.Second)
	if err := profile.RunOnce(context.Background()); !errors.Is(err, ErrIdentityRecoveryClaimLost) || store.resetClaims != 1 {
		t.Fatal("profile worker dispatched without a claim or skipped independent password recovery", err)
	}
}
