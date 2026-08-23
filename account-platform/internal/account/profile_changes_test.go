package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

type profileStoreFake struct {
	change ProfileChange
	calls  []string
}

func (f *profileStoreFake) CreateProfileChange(_ context.Context, p identity.Principal, field, value string) (ProfileChange, error) {
	f.change = ProfileChange{ID: uuid.New(), UserID: uuid.New(), IdentityIssuer: p.Issuer, IdentitySubject: p.Subject, Field: field, OldValue: "old-name", NewValue: value, ExpectedProfileVersion: 3, Status: "PENDING", SLAExpiresAt: time.Now().Add(time.Hour)}
	return f.change, nil
}
func (f *profileStoreFake) ProfileChange(context.Context, uuid.UUID, *uuid.UUID) (ProfileChange, error) {
	return f.change, nil
}
func (*profileStoreFake) ActiveProfileSMSChallenge(context.Context, uuid.UUID) (*SMSChallenge, error) {
	return nil, nil
}
func (*profileStoreFake) BindProfileSMSChallenge(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) error {
	return nil
}
func (*profileStoreFake) MarkProfileSMSVerified(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}
func (f *profileStoreFake) MarkProfileIdentityUpdated(context.Context, uuid.UUID) error {
	now := time.Now()
	f.change.IdentityUpdatedAt = &now
	f.change.Status = "IDENTITY_UPDATED"
	f.calls = append(f.calls, "identity")
	return nil
}
func (f *profileStoreFake) CommitProfileChange(context.Context, uuid.UUID) error {
	now := time.Now()
	f.change.AccountCommittedAt = &now
	f.change.Status = "ACCOUNT_COMMITTED"
	f.calls = append(f.calls, "account")
	return nil
}
func (f *profileStoreFake) MarkProfileSessionsRevoked(context.Context, uuid.UUID) error {
	now := time.Now()
	f.change.SessionsRevokedAt = &now
	f.calls = append(f.calls, "sessions")
	return nil
}
func (f *profileStoreFake) CompleteProfileChange(context.Context, uuid.UUID) error {
	f.change.Status = "COMPLETED"
	f.calls = append(f.calls, "complete")
	return nil
}
func (*profileStoreFake) ScheduleProfileChangeRetry(context.Context, uuid.UUID, string) error {
	return nil
}
func (*profileStoreFake) ClaimProfileChange(context.Context) (*ProfileChange, error) { return nil, nil }
func (*profileStoreFake) CreatePasswordReset(context.Context, string, string, uuid.UUID) (uuid.UUID, uuid.UUID, error) {
	return uuid.New(), uuid.New(), nil
}
func (*profileStoreFake) CompletePasswordReset(context.Context, uuid.UUID, string) error { return nil }
func (*profileStoreFake) ListProfileRepairs(context.Context) ([]profileChangeRepairItem, error) {
	return nil, nil
}
func (*profileStoreFake) RetryProfileRepair(context.Context, identity.Principal, uuid.UUID) error {
	return nil
}

type profileIdentityFake struct{ calls []string }

func (*profileIdentityFake) ValidateMobileUser(context.Context, uuid.UUID, string, string, string, string) error {
	return nil
}
func (*profileIdentityFake) CreateMobileUser(context.Context, uuid.UUID, string, string, string, string) (string, error) {
	return "", nil
}
func (*profileIdentityFake) FindMobileUserByRegistrationOperation(context.Context, uuid.UUID) (string, bool, error) {
	return "", false, nil
}
func (*profileIdentityFake) DisableUserAndConfirm(context.Context, string) error { return nil }
func (*profileIdentityFake) DeleteUser(context.Context, string) error            { return nil }
func (f *profileIdentityFake) UpdateUsername(context.Context, string, uuid.UUID, string) error {
	f.calls = append(f.calls, "identity")
	return nil
}
func (*profileIdentityFake) UpdateMobile(context.Context, string, uuid.UUID, string) error {
	return nil
}
func (*profileIdentityFake) ResetPassword(context.Context, string, uuid.UUID, string, string, string) error {
	return nil
}
func (f *profileIdentityFake) RevokeSessions(context.Context, string) error {
	f.calls = append(f.calls, "sessions")
	return nil
}

func TestUsernameChangeRequiresRecentAuthenticationAndConvergesForward(t *testing.T) {
	now := time.Date(2026, time.August, 23, 4, 0, 0, 0, time.UTC)
	store := &profileStoreFake{}
	identities := &profileIdentityFake{}
	service := &ProfileChangeService{store: store, identities: identities, now: func() time.Time { return now }}
	stale := identity.Principal{Issuer: "issuer", Subject: "subject", AuthenticatedAt: now.Add(-6 * time.Minute)}
	if _, err := service.Create(context.Background(), stale, "USERNAME", "new-name"); !errors.Is(err, ErrRecentAuthenticationRequired) {
		t.Fatalf("stale authentication error=%v", err)
	}
	principal := identity.Principal{Issuer: "issuer", Subject: "subject", AuthenticatedAt: now.Add(-time.Minute)}
	change, err := service.Create(context.Background(), principal, "USERNAME", "new-name")
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Complete(context.Background(), principal, change.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "COMPLETED" {
		t.Fatalf("status=%s", result.Status)
	}
	if len(identities.calls) != 2 || identities.calls[0] != "identity" || identities.calls[1] != "sessions" {
		t.Fatalf("identity calls=%v", identities.calls)
	}
	if len(store.calls) != 4 || store.calls[0] != "identity" || store.calls[1] != "account" || store.calls[2] != "sessions" || store.calls[3] != "complete" {
		t.Fatalf("store calls=%v", store.calls)
	}
}
