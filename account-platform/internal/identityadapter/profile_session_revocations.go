package identityadapter

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/profilerevocation"
	"github.com/google/uuid"
)

var errProfileRevocationUnconfirmed = profilerevocation.ErrUnconfirmed
var errProfileRevocationConflict = profilerevocation.ErrConflict

type profileSessionRevocations struct {
	directory UserDirectory
	authority profilerevocation.Authority
}

func newProfileSessionRevocations(directory UserDirectory) *profileSessionRevocations {
	return &profileSessionRevocations{directory: directory}
}

func (s *Service) WithProfileRevocationAuthority(authority profilerevocation.Authority) (*Service, error) {
	if authority == nil {
		return nil, errors.New("profile revocation receipt authority is required")
	}
	s.profileRevocations.authority = authority
	return s, nil
}

// The Account receipt owns admission, dispatch and confirmation across adapter
// processes. No process-local operation history or expiring replay cache is used.
func (v *profileSessionRevocations) revoke(ctx context.Context, operationID uuid.UUID, subject string, replayOnly bool) error {
	if operationID == uuid.Nil || strings.TrimSpace(subject) == "" || len(subject) > 128 {
		return errProfileRevocationConflict
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if v.authority == nil {
		return errProfileRevocationUnconfirmed
	}
	command := profilerevocation.Command{OperationID: operationID, Subject: subject, DispatchToken: uuid.New(), ReplayOnly: replayOnly}
	result, err := v.authority.ClaimProfileRevocation(ctx, command)
	if err != nil {
		return err
	}
	if result.State == "CONFIRMED" {
		return nil
	}
	if result.State != "DISPATCH" || replayOnly {
		return errProfileRevocationUnconfirmed
	}
	if err = ctx.Err(); err != nil {
		return errProfileRevocationUnconfirmed
	}
	if err = v.directory.RevokeSessions(ctx, subject); err != nil {
		return errProfileRevocationUnconfirmed
	}
	// A completed provider effect can still be confirmed after the caller leaves.
	// Confirmation is bounded and never authorizes a second provider dispatch.
	confirmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err = v.authority.ConfirmProfileRevocation(confirmCtx, command); err != nil {
		return errProfileRevocationUnconfirmed
	}
	return nil
}

func (s *Service) revokeProfileSessions(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OperationID     uuid.UUID `json:"operation_id"`
		FirstAttempt    *bool     `json:"first_attempt"`
		ReceiptProtocol *int      `json:"receipt_protocol"`
	}
	if api.DecodeJSON(w, r, &body) != nil || body.OperationID == uuid.Nil || body.FirstAttempt == nil || body.ReceiptProtocol != nil && *body.ReceiptProtocol != 2 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "a profile operation and first-attempt decision are required", httpserver.RequestID(r.Context()))
		return
	}
	err := s.profileRevocations.revoke(r.Context(), body.OperationID, r.PathValue("subject"), body.ReceiptProtocol == nil)
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
