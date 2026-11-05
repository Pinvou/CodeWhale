package identityadapter

import (
	"errors"
	"github.com/google/uuid"
)

func (v *PasswordVault) registrationDispatchFence(id uuid.UUID, handle, policy, username, mobile string, first bool) error {
	v.operationsMu.Lock()
	defer v.operationsMu.Unlock()
	v.removeExpiredOperationsLocked()
	if b, ok := v.operations[id]; ok {
		if b.username != username || b.mobile != mobile || (handle != "" && b.handle != handle) {
			return ErrReferenceConflict
		}
		if policy != PasswordPolicyVersion {
			return ErrReferenceInvalid
		}
		if b.cancelled {
			if first {
				return ErrCreateNotDispatched
			}
			return ErrReferenceInvalid
		}
	}
	return nil
}

// No lookup, expired handle or lost process state proves absence. Only a
// matching retained first binding can be cancelled before any remote dispatch.
func (v *PasswordVault) registrationReferenceError(id uuid.UUID, handle, policy, username, mobile string, first bool, cause error) error {
	if !first || policy != PasswordPolicyVersion || !errors.Is(cause, ErrReferenceInvalid) {
		return cause
	}
	v.operationsMu.Lock()
	defer v.operationsMu.Unlock()
	v.removeExpiredOperationsLocked()
	b, ok := v.operations[id]
	if !ok || b.dispatched || b.username != username || b.mobile != mobile || b.handle != handle {
		return cause
	}
	b.cancelled = true
	v.operations[id] = b
	return ErrCreateNotDispatched
}

func (v *PasswordVault) markRegistrationDispatch(id uuid.UUID, username, mobile string) error {
	v.operationsMu.Lock()
	defer v.operationsMu.Unlock()
	v.removeExpiredOperationsLocked()
	b, ok := v.operations[id]
	if !ok {
		return ErrCreateUnconfirmed
	}
	if b.username != username || b.mobile != mobile {
		return ErrReferenceConflict
	}
	if b.dispatched || b.cancelled {
		return ErrCreateUnconfirmed
	}
	b.dispatched = true
	v.operations[id] = b
	return nil
}
