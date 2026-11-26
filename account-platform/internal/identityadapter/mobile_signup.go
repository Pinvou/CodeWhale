package identityadapter

import (
	"context"
	"regexp"
	"time"

	"github.com/google/uuid"
)

var (
	smsOnlyUsername = regexp.MustCompile(`^user-[a-f0-9]{24}$`)
	smsOnlyMobile   = regexp.MustCompile(`^\+861[3-9][0-9]{9}$`)
)

func validSMSOnlyIdentity(username, mobile string) bool {
	return smsOnlyUsername.MatchString(username) && smsOnlyMobile.MatchString(mobile)
}

func (v *PasswordVault) createSMSOnlyUser(ctx context.Context, id uuid.UUID, username, mobile string, firstAttempt bool) (string, error) {
	if !validSMSOnlyIdentity(username, mobile) {
		return "", ErrReferenceInvalid
	}
	if !firstAttempt {
		if subject, found, err := v.directory.FindMobileUserByRegistrationOperation(ctx, id); err != nil || found {
			return subject, err
		}
		return "", ErrCreateUnconfirmed
	}
	// Account performed correlation before issuing its durable first grant.
	// A second remote lookup here could fail and strand a never-created user.
	if err := v.markSMSOnlyDispatch(id, username, mobile); err != nil {
		return "", err
	}
	return v.directory.CreateMobileUser(ctx, id, username, mobile, "")
}

func (v *PasswordVault) markSMSOnlyDispatch(id uuid.UUID, username, mobile string) error {
	v.operationsMu.Lock()
	defer v.operationsMu.Unlock()
	v.removeExpiredOperationsLocked()
	if binding, ok := v.operations[id]; ok {
		if binding.handle != "" || binding.username != username || binding.mobile != mobile {
			return ErrReferenceConflict
		}
		return ErrCreateUnconfirmed
	}
	expires := v.now().Add(48 * time.Hour)
	v.operations[id] = operationPasswordBinding{username: username, mobile: mobile, expiresAt: expires, dispatched: true}
	v.proofExpiry.add(id, expires)
	return nil
}
