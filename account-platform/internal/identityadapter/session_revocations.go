package identityadapter

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/google/uuid"
)

var (
	ErrSessionRevocationUnconfirmed = errors.New("session revocation has no confirmed receipt")
	ErrSessionRevocationConflict    = errors.New("session revocation belongs to another identity")
)

type sessionRevocationReceipt struct {
	mu         sync.Mutex
	subject    string
	dispatched bool
	confirmed  bool
}

// SessionRevocations owns profile-operation revocation acknowledgements, apart
// from password references. Account durably grants firstAttempt exactly once.
// Receipts live for this service process and are never expired into a new call:
// after restart, a non-first request with no receipt fails closed for repair.
type SessionRevocations struct {
	mu        sync.Mutex
	receipts  map[uuid.UUID]*sessionRevocationReceipt
	directory UserDirectory
}

func newSessionRevocations(directory UserDirectory) *SessionRevocations {
	return &SessionRevocations{receipts: make(map[uuid.UUID]*sessionRevocationReceipt), directory: directory}
}

func (v *SessionRevocations) Revoke(ctx context.Context, subject string, operationID uuid.UUID, firstAttempt bool) error {
	if operationID == uuid.Nil || strings.TrimSpace(subject) == "" || len(subject) > 128 {
		return ErrSessionRevocationConflict
	}
	v.mu.Lock()
	receipt, exists := v.receipts[operationID]
	if !exists {
		if !firstAttempt {
			v.mu.Unlock()
			return ErrSessionRevocationUnconfirmed
		}
		receipt = &sessionRevocationReceipt{subject: subject}
		v.receipts[operationID] = receipt
	}
	v.mu.Unlock()
	receipt.mu.Lock()
	defer receipt.mu.Unlock()
	if receipt.subject != subject {
		return ErrSessionRevocationConflict
	}
	if receipt.confirmed {
		return nil
	}
	if receipt.dispatched {
		// Repeating an unknown logout could revoke a newly-created login.
		return ErrSessionRevocationUnconfirmed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	receipt.dispatched = true
	if err := v.directory.RevokeSessions(ctx, subject); err != nil {
		return ErrSessionRevocationUnconfirmed
	}
	receipt.confirmed = true
	return nil
}
