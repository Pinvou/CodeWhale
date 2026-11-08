package identityadapter

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

var errProfileRevocationUnconfirmed = errors.New("profile revocation has no confirmed receipt")
var errProfileRevocationConflict = errors.New("profile revocation belongs to another subject")

type profileRevocationReceipt struct {
	mu         sync.Mutex
	subject    string
	dispatched bool
	confirmed  bool
}

type profileSessionRevocations struct {
	mu        sync.Mutex
	receipts  map[uuid.UUID]*profileRevocationReceipt
	directory UserDirectory
}

func newProfileSessionRevocations(directory UserDirectory) *profileSessionRevocations {
	return &profileSessionRevocations{receipts: make(map[uuid.UUID]*profileRevocationReceipt), directory: directory}
}

// Account persists the first-attempt grant. Receipts intentionally live only for
// this adapter process: after restart a non-first unknown command requires repair.
func (v *profileSessionRevocations) revoke(ctx context.Context, operationID uuid.UUID, subject string, first bool) error {
	if operationID == uuid.Nil || strings.TrimSpace(subject) == "" || len(subject) > 128 {
		return errProfileRevocationConflict
	}
	v.mu.Lock()
	receipt := v.receipts[operationID]
	if receipt == nil {
		if !first {
			v.mu.Unlock()
			return errProfileRevocationUnconfirmed
		}
		receipt = &profileRevocationReceipt{subject: subject}
		v.receipts[operationID] = receipt
	}
	v.mu.Unlock()
	receipt.mu.Lock()
	defer receipt.mu.Unlock()
	if receipt.subject != subject {
		return errProfileRevocationConflict
	}
	if receipt.confirmed {
		return nil
	}
	if receipt.dispatched {
		return errProfileRevocationUnconfirmed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	receipt.dispatched = true
	if err := v.directory.RevokeSessions(ctx, subject); err != nil {
		return errProfileRevocationUnconfirmed
	}
	receipt.confirmed = true
	return nil
}

func (s *Service) revokeProfileSessions(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OperationID  uuid.UUID `json:"operation_id"`
		FirstAttempt *bool     `json:"first_attempt"`
	}
	if api.DecodeJSON(w, r, &body) != nil || body.OperationID == uuid.Nil || body.FirstAttempt == nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "a profile operation and first-attempt decision are required", httpserver.RequestID(r.Context()))
		return
	}
	err := s.profileRevocations.revoke(r.Context(), body.OperationID, r.PathValue("subject"), *body.FirstAttempt)
	if errors.Is(err, errProfileRevocationUnconfirmed) {
		api.WriteError(w, http.StatusServiceUnavailable, "PROFILE_REVOCATION_UNCONFIRMED", "the profile revocation requires a confirmed receipt or operator review", httpserver.RequestID(r.Context()))
		return
	}
	if errors.Is(err, errProfileRevocationConflict) {
		api.WriteError(w, http.StatusConflict, "PROFILE_REVOCATION_CONFLICT", "the profile revocation belongs to another identity", httpserver.RequestID(r.Context()))
		return
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
