package identityadapter

import (
	"context"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// A receipt stores no password. It shares the existing bounded operation
// lifetime so a confirmed change can finish revocation after the secret expires.
// An acknowledged completion must not revoke sessions created by a later login.
type passwordResetReceipt struct {
	mu          sync.Mutex
	passwordSet bool
	completed   bool
}

func (v *PasswordVault) resetReceipt(operationID uuid.UUID, handle, policyVersion, subject, mobile string, create bool) (*passwordResetReceipt, error) {
	if operationID == uuid.Nil || len(handle) != 43 || policyVersion != PasswordPolicyVersion || strings.TrimSpace(subject) == "" || mobile == "" {
		return nil, ErrReferenceInvalid
	}
	v.operationsMu.Lock()
	defer v.operationsMu.Unlock()
	v.removeExpiredOperationsLocked()
	binding, ok := v.operations[operationID]
	if !ok {
		if create {
			return nil, ErrReferenceInvalid
		}
		return nil, nil
	}
	if binding.username != subject || binding.mobile != mobile || binding.handle != handle || binding.dispatched || binding.cancelled {
		return nil, ErrReferenceConflict
	}
	if binding.reset == nil && create {
		binding.reset = &passwordResetReceipt{}
		v.operations[operationID] = binding
	}
	return binding.reset, nil
}

func (v *PasswordVault) ResetPassword(ctx context.Context, operationID uuid.UUID, handle, policyVersion, subject, mobile string) error {
	receipt, err := v.resetReceipt(operationID, handle, policyVersion, subject, mobile, false)
	if err != nil {
		return err
	}
	if receipt == nil {
		if err := v.ValidateReference(operationID, handle, policyVersion, subject, mobile); err != nil {
			return err
		}
		receipt, err = v.resetReceipt(operationID, handle, policyVersion, subject, mobile, true)
		if err != nil {
			return err
		}
	}
	receipt.mu.Lock()
	defer receipt.mu.Unlock()
	if receipt.completed {
		return nil
	}
	if !receipt.passwordSet {
		record, err := v.record(handle, policyVersion)
		if err != nil {
			return err
		}
		record.mu.Lock()
		err = v.setResetPassword(ctx, record, operationID, subject, mobile)
		if err == nil {
			receipt.passwordSet = true
			record.password = ""
		}
		record.mu.Unlock()
		if err != nil {
			return err
		}
	}
	if err := v.directory.RevokeSessions(ctx, subject); err != nil {
		return err
	}
	receipt.completed = true
	return nil
}

// Caller holds both this operation receipt and the password record.
func (v *PasswordVault) setResetPassword(ctx context.Context, record *passwordRecord, operationID uuid.UUID, subject, mobile string) error {
	if err := bindRecord(record, subject, mobile); err != nil {
		return err
	}
	if v.now().After(record.expiresAt) {
		return ErrReferenceInvalid
	}
	if record.operationID != uuid.Nil && record.operationID != operationID {
		return ErrReferenceConflict
	}
	if err := ValidatePassword(record.password); err != nil {
		return err
	}
	record.operationID = operationID
	return v.directory.SetPassword(ctx, subject, record.password)
}
